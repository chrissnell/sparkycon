package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/dustin/randbo"
	"github.com/gizak/termui"
)

const (
	blockSize        int64  = 50
	reportIntervalMS uint64 = 300 // report interval in milliseconds
	testLength       uint   = 10
)

// testType is used to indicate the type of test being performed
type testType int

const (
	outbound testType = iota
	inbound
	echo
)

type meteredClient struct {
	blockTicker        chan bool
	throughputReport   chan float64
	measurerDone       chan struct{}
	dlDone             chan struct{}
	ulDone             chan struct{}
	statsGeneratorDone chan struct{}
	dlReport           chan float64
	ulReport           chan float64
	wr                 *widgetRenderer
	rendererMu         *sync.Mutex
}

type widgetRenderer struct {
	jobs map[string]termui.Bufferer
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: ", os.Args[0], " <sparkyfish IP:port>")
	}

	// Initialize our screen
	err := termui.Init()
	if err != nil {
		panic(err)
	}
	defer termui.Close()

	// 'q' quits the program
	termui.Handle("/sys/kbd/q", func(termui.Event) {
		termui.StopLoop()
	})

	mc := newMeteredClient()
	mc.wr = newwidgetRenderer()

	// Begin our tests
	go mc.runThroughputTestSequence()

	termui.Loop()
}

func (mc *meteredClient) runThroughputTestSequence() {
	// First, we need to build the widgets on our screen.

	// Build our title box
	titleBox := termui.NewPar("-----[ sparkyfish ]-------------------------------")
	titleBox.Height = 1
	titleBox.Width = 50
	titleBox.Y = 0
	titleBox.Border = false
	titleBox.TextFgColor = termui.ColorWhite | termui.AttrBold

	// Build a download graph widget
	dlGraph := termui.NewLineChart()
	dlGraph.BorderLabel = " Download Throughput "
	dlGraph.Width = 25
	dlGraph.Height = 12
	dlGraph.X = 0
	dlGraph.Y = 1
	// Windows Command Prompt doesn't support our Unicode characters with the default font
	if runtime.GOOS == "windows" {
		dlGraph.Mode = "dot"
		dlGraph.DotStyle = '+'
	}
	dlGraph.AxesColor = termui.ColorWhite
	dlGraph.LineColor = termui.ColorGreen | termui.AttrBold

	// Build an upload graph widget
	ulGraph := termui.NewLineChart()
	ulGraph.BorderLabel = " Upload Throughput "
	ulGraph.Data = []float64{0}
	ulGraph.Width = 25
	ulGraph.Height = 12
	ulGraph.X = 25
	ulGraph.Y = 1
	// Windows Command Prompt doesn't support our Unicode characters with the default font
	if runtime.GOOS == "windows" {
		ulGraph.Mode = "dot"
		ulGraph.DotStyle = '+'
	}
	ulGraph.AxesColor = termui.ColorWhite
	ulGraph.LineColor = termui.ColorGreen | termui.AttrBold

	// Build a stats summary widget
	statsSummary := termui.NewPar("")
	statsSummary.Height = 7
	statsSummary.Width = 50
	statsSummary.Y = 13
	statsSummary.BorderLabel = " Tests Summary "
	statsSummary.TextFgColor = termui.ColorWhite | termui.AttrBold

	// Build out progress gauge widget
	progress := termui.NewGauge()
	progress.Percent = 40
	progress.Width = 50
	progress.Height = 3
	progress.Y = 20
	progress.BorderLabel = " Test Progress "
	progress.Percent = 0
	progress.BarColor = termui.ColorRed
	progress.BorderFg = termui.ColorWhite
	progress.PercentColorHighlighted = termui.ColorWhite | termui.AttrBold
	progress.PercentColor = termui.ColorWhite | termui.AttrBold

	// Build our helpbox widget
	helpBox := termui.NewPar(" COMMANDS: [q]uit")
	helpBox.Height = 1
	helpBox.Width = 50
	helpBox.Y = 23
	helpBox.Border = false
	helpBox.TextBgColor = termui.ColorBlue
	helpBox.TextFgColor = termui.ColorWhite | termui.AttrBold
	helpBox.Bg = termui.ColorBlue

	// Add the widgets to the rendering jobs and render the screen
	mc.wr.Add("titlebox", titleBox)
	mc.wr.Add("dlgraph", dlGraph)
	mc.wr.Add("ulgraph", ulGraph)
	mc.wr.Add("statsSummary", statsSummary)
	mc.wr.Add("progress", progress)
	mc.wr.Add("helpbox", helpBox)
	mc.wr.Render()

	// Prepare some channels that we'll use to send throughput
	// reports to the stats summary widget.
	mc.dlReport = make(chan float64)
	mc.ulReport = make(chan float64)
	mc.dlDone = make(chan struct{})
	mc.ulDone = make(chan struct{})
	mc.statsGeneratorDone = make(chan struct{})

	// Start our stats generator, which receives realtime measurements from the throughput
	// reporter and generates metrics from them
	go mc.generateStats()

	// Start our d/l throughput reporter to receive throughput measurements and send
	// them on to be rendered in our stats widget
	go mc.ReportThroughputDL()

	// Run our download tests and block until that's done
	mc.runThroughputTest(inbound)

	// Signal to the throughput reporter that the download test is complete
	close(mc.dlDone)

	// Start our u/l throughput reporter to receive throughput measurements and send
	// them on to be rendered in our stats widget
	go mc.ReportThroughputUL()

	// Run an outbound (upload) throughput test and block until it's complete
	mc.runThroughputTest(outbound)

	// Signal to our generators that the upload test is complete
	close(mc.statsGeneratorDone)
	close(mc.ulDone)

	return
}

// Kick off a throughput measurement test
func (mc *meteredClient) runThroughputTest(dir testType) {
	// Used to signal test completion to the throughput measurer
	measurerDone := make(chan struct{})

	// Launch a progress bar updater
	go mc.updateProgressBar()

	// Launch a throughput measurer and then kick off the metered copy,
	// blocking until it completes.
	go mc.MeasureThroughput(measurerDone)
	mc.MeteredCopy(dir, measurerDone)
}

// NewMeteredClient creates a new MeteredClient object
func newMeteredClient() *meteredClient {
	m := meteredClient{}
	m.blockTicker = make(chan bool)
	m.throughputReport = make(chan float64)
	return &m
}

// Kicks off a metered copy (throughput test) by sending a command to the server
// and then performing the appropriate I/O copy, sending "ticks" by channel as
// each block of data passes through.
func (mc *meteredClient) MeteredCopy(dir testType, measurerDone chan<- struct{}) {
	var rnd io.Reader
	var tl time.Duration

	// Connect to the remote sparkyfish server
	conn, err := net.Dial("tcp", os.Args[1])
	if err != nil {
		termui.Close()
		log.Fatalln(err)
	}

	defer conn.Close()

	// Send the appropriate command to the sparkyfish server to initiate our
	// throughput test
	switch dir {
	case inbound:
		// For inbound tests, we bump our timer by 2 seconds to account for
		// the remote server's test startup time
		tl = time.Second * time.Duration(testLength+2)
		_, err = conn.Write([]byte("SND"))
		if err != nil {
			termui.Close()
			log.Fatalln(err)
		}
	case outbound:
		tl = time.Second * time.Duration(testLength)
		_, err = conn.Write([]byte("RCV"))
		if err != nil {
			termui.Close()
			log.Fatalln(err)
		}
		// Create a new randbo Reader, used to generate our random data that we'll upload
		rnd = randbo.New()
	}

	// Set a timer for running the tests
	timer := time.NewTimer(tl)

	switch dir {
	case inbound:
		// Receive, tally, and discard incoming data as fast as we can until the sender stops sending or the timer expires
		for {
			select {
			case <-timer.C:
				// Timer has elapsed and test is finished
				close(measurerDone)
				return
			default:
				// Copy data from our net.Conn to the rubbish bin in (blockSize) KB chunks
				_, err = io.CopyN(ioutil.Discard, conn, 1024*blockSize)
				if err != nil {
					if err == io.EOF {
						close(measurerDone)
						return
					}
					log.Println("Error copying:", err)
					return
				}
				// With each chunk copied, we send a message on our blockTicker channel
				mc.blockTicker <- true

			}
		}
	case outbound:
		// Send and tally outgoing data as fast as we can until the receiver stops receiving or the timer expires
		for {
			select {
			case <-timer.C:
				// Timer has elapsed and test is finished
				close(measurerDone)
				return
			default:
				// Copy data from our RNG to the net.Conn in (blockSize) KB chunks
				_, err = io.CopyN(conn, rnd, 1024*blockSize)
				if err != nil {
					if err == io.EOF {
						close(measurerDone)
						return
					}
					log.Println("Error copying:", err)
					return
				}
				// With each chunk copied, we send a message on our blockTicker channel
				mc.blockTicker <- true
			}
		}
	}
}

// MeasureThroughput receives ticks sent by MeteredCopy() and derives a throughput rate, which is then sent
// to the throughput reporter.
func (mc *meteredClient) MeasureThroughput(measurerDone <-chan struct{}) {
	var blockCount, prevBlockCount uint64

	tick := time.NewTicker(time.Duration(reportIntervalMS) * time.Millisecond)
	for {
		select {
		case <-mc.blockTicker:
			// Increment our block counter when we get a ticker
			blockCount++
		case <-measurerDone:
			tick.Stop()
			return
		case <-tick.C:
			// Every second, we calculate how many blocks were received
			// and derive an average throughput rate.
			mc.throughputReport <- (float64(blockCount - prevBlockCount)) * float64(blockSize*8) / float64(reportIntervalMS)

			prevBlockCount = blockCount
		}
	}
}

// ReportThroughputDL receives throughput reports for the download test
// and updates the bar chart accordingly. It also passes them on to the
// stats generator.
func (mc *meteredClient) ReportThroughputDL() {
	var dlReadings []float64

	for {
		select {
		case r := <-mc.throughputReport:
			if len(dlReadings) >= 70 {
				dlReadings = dlReadings[1:]
			}
			dlReadings = append(dlReadings, r)
			mc.dlReport <- r
			mc.wr.jobs["dlgraph"].(*termui.LineChart).Data = dlReadings
		case <-mc.dlDone:
			return
		}
	}
}

// ReportThroughputDL receives throughput reports for the upload test
// and updates the bar chart accordingly. It also passes them on to the
// stats generator.
func (mc *meteredClient) ReportThroughputUL() {
	var ulReadings []float64

	for {
		select {
		case r := <-mc.throughputReport:
			if len(ulReadings) >= 70 {
				ulReadings = ulReadings[1:]
			}
			ulReadings = append(ulReadings, r)
			mc.ulReport <- r
			mc.wr.jobs["ulgraph"].(*termui.LineChart).Data = ulReadings
		case <-mc.ulDone:
			return
		}
	}
}

// generateStats receives download and upload speed reports and computes metrics
// which are displayed in the stats widget.
func (mc *meteredClient) generateStats() {
	var currentDL, maxDL, avgDL float64
	var currentUL, maxUL, avgUL float64
	var dlReadingCount, dlReadingSum float64
	var ulReadingCount, ulReadingSum float64

	for {
		select {
		case currentDL = <-mc.dlReport:
			dlReadingCount++
			dlReadingSum = dlReadingSum + currentDL
			avgDL = dlReadingSum / dlReadingCount
			if currentDL > maxDL {
				maxDL = currentDL
			}
			// Update our stats widget with the latest readings
			mc.wr.jobs["statsSummary"].(*termui.Par).Text = fmt.Sprintf("DOWNLOAD \nCurrent: %v Mbps\tMax: %v\tAvg: %v\n\nUPLOAD\nCurrent: %v Mbps\tMax: %v\tAvg: %v",
				strconv.FormatFloat(currentDL, 'f', 1, 64), strconv.FormatFloat(maxDL, 'f', 1, 64), strconv.FormatFloat(avgDL, 'f', 1, 64),
				strconv.FormatFloat(currentUL, 'f', 1, 64), strconv.FormatFloat(maxUL, 'f', 1, 64), strconv.FormatFloat(avgUL, 'f', 1, 64))
			mc.wr.Render()
		case currentUL = <-mc.ulReport:
			ulReadingCount++
			ulReadingSum = ulReadingSum + currentUL
			avgUL = ulReadingSum / ulReadingCount
			if currentUL > maxUL {
				maxUL = currentUL
			}
			// Update our stats widget with the latest readings
			mc.wr.jobs["statsSummary"].(*termui.Par).Text = fmt.Sprintf("DOWNLOAD \nCurrent: %v Mbps\tMax: %v\tAvg: %v\n\nUPLOAD\nCurrent: %v Mbps\tMax: %v\tAvg: %v",
				strconv.FormatFloat(currentDL, 'f', 1, 64), strconv.FormatFloat(maxDL, 'f', 1, 64), strconv.FormatFloat(avgDL, 'f', 1, 64),
				strconv.FormatFloat(currentUL, 'f', 1, 64), strconv.FormatFloat(maxUL, 'f', 1, 64), strconv.FormatFloat(avgUL, 'f', 1, 64))
			mc.wr.Render()
		case <-mc.statsGeneratorDone:
			return
		}
	}
}

// updateProgressBar updates the progress bar as tests run
func (mc *meteredClient) updateProgressBar() {
	var updateIntervalMS uint = 500
	var progress uint

	mc.wr.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorRed

	//progressPerUpdate := testLength / (updateIntervalMS / 1000)
	var progressPerUpdate uint = 100 / 20

	// Set a ticker for updating the progress BarColor
	tick := time.NewTicker(time.Duration(updateIntervalMS) * time.Millisecond)

	// Set a timer to
	timer := time.NewTimer(time.Second * time.Duration(testLength))

	for {
		select {
		case <-tick.C:
			progress = progress + progressPerUpdate
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = int(progress)
			mc.wr.Render()
		case <-timer.C:
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = 100
			mc.wr.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorGreen
			mc.wr.Render()
			return
		}
	}

}

func newwidgetRenderer() *widgetRenderer {
	wr := widgetRenderer{}
	wr.jobs = make(map[string]termui.Bufferer)
	return &wr
}

func (wr *widgetRenderer) Add(name string, job termui.Bufferer) {
	wr.jobs[name] = job
}

func (wr *widgetRenderer) Delete(name string) {
	delete(wr.jobs, name)
}

func (wr *widgetRenderer) Render() {
	var jobs []termui.Bufferer
	for _, j := range wr.jobs {
		jobs = append(jobs, j)
	}
	termui.Render(jobs...)
}
