package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dustin/randbo"
	"github.com/gizak/termui"
)

const (
	blockSize        int64  = 50
	reportIntervalMS uint64 = 200 // report interval in milliseconds
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
	rj                 *renderJobs
	rendererMu         *sync.Mutex
}

type renderJobs struct {
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
	mc.rj = newRenderJobs()

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
	throughputGraph := termui.NewLineChart()
	throughputGraph.BorderLabel = " Download Throughput "
	throughputGraph.Width = 50
	throughputGraph.Height = 12
	throughputGraph.X = 0
	throughputGraph.Y = 1
	//throughputGraph.Mode = "dot"
	//throughputGraph.DotStyle = '+'
	throughputGraph.AxesColor = termui.ColorWhite
	throughputGraph.LineColor = termui.ColorGreen | termui.AttrBold

	// Build a stats summary widget
	statsSummary := termui.NewPar("Borderless Text")
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
	mc.rj.Add("titlebox", titleBox)
	mc.rj.Add("throughputGraph", throughputGraph)
	mc.rj.Add("statsSummary", statsSummary)
	mc.rj.Add("progress", progress)
	mc.rj.Add("helpbox", helpBox)
	mc.rj.Render()

	// Prepare some channels that we'll use to send throughput
	// reports to the stats summary widget.
	mc.dlReport = make(chan float64)
	mc.ulReport = make(chan float64)
	mc.dlDone = make(chan struct{})
	mc.ulDone = make(chan struct{})
	mc.statsGeneratorDone = make(chan struct{})

	// Start our d/l throughput reporter to receive throughput measurements and send
	// them on to be rendered in our stats widget
	go mc.ReportThroughputDL()
	go mc.generateStats()

	// Run our download tests and block until that's done
	mc.runThroughputTest(inbound)

	// Signal to the throughput reporter that the download test is complete
	close(mc.dlDone)

	// To reset the Y-axis on our throughput graph, we have to delete
	// the widget and then add the widget back before rendering again
	mc.rj.Delete("throughputGraph")
	throughputGraph = termui.NewLineChart()
	throughputGraph.BorderLabel = " Upload Throughput "
	throughputGraph.Width = 50
	throughputGraph.Height = 12
	throughputGraph.X = 0
	throughputGraph.Y = 1
	// throughputGraph.Mode = "dot"
	// throughputGraph.DotStyle = '+'
	throughputGraph.AxesColor = termui.ColorWhite
	throughputGraph.LineColor = termui.ColorRed | termui.AttrBold
	mc.rj.Add("throughputGraph", throughputGraph)
	mc.rj.Render()

	// Start our u/l throughput reporter to receive throughput measurements and send
	// them on to be rendered in our stats widget
	go mc.ReportThroughputUL()

	mc.runThroughputTest(outbound)

	close(mc.statsGeneratorDone)

	// Signal to the throughput reporter that the upload test is complete
	close(mc.ulDone)

	return
}

// Kick off a throughput measurement test
func (mc *meteredClient) runThroughputTest(dir testType) {
	// Used to signal test completion to the throughput measurer
	measurerDone := make(chan struct{})

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
		// Create a new randbo Reader
		rnd = randbo.New()
	}

	// Set a timer for running the tests
	timer := time.NewTimer(tl)

	switch dir {
	case inbound:
		for {
			select {
			case <-timer.C:
				// Timer has elapsed and test is finished
				close(measurerDone)
				return
			default:
				_, err = io.CopyN(ioutil.Discard, conn, 1024*blockSize)
				if err != nil {
					if err == io.EOF {
						close(measurerDone)
						return
					}
					log.Println("Error copying:", err)
					return
				}
				// With each 100K copied, we send a message on our blockTicker channel
				mc.blockTicker <- true

			}
		}
	case outbound:
		for {
			select {
			case <-timer.C:
				// Timer has elapsed and test is finished
				close(measurerDone)
				return
			default:
				_, err = io.CopyN(conn, rnd, 1024*blockSize)
				if err != nil {
					if err == io.EOF {
						close(measurerDone)
						return
					}
					log.Println("Error copying:", err)
					return
				}
				// With each 100K copied, we send a message on our blockTicker channel
				mc.blockTicker <- true
			}
		}
	}
}

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
			mc.rj.jobs["throughputGraph"].(*termui.LineChart).Data = dlReadings
		case <-mc.dlDone:
			return
		}
	}
}

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
			mc.rj.jobs["throughputGraph"].(*termui.LineChart).Data = ulReadings
		case <-mc.ulDone:
			return
		}
	}
}

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
			mc.rj.jobs["statsSummary"].(*termui.Par).Text = fmt.Sprintf("DOWNLOAD \nCurrent: %v Mbps\tMax: %v\tAvg: %v\n\nUPLOAD\nCurrent: %v Mbps\tMax: %v\tAvg: %v",
				strconv.FormatFloat(currentDL, 'f', 1, 64), strconv.FormatFloat(maxDL, 'f', 1, 64), strconv.FormatFloat(avgDL, 'f', 1, 64),
				strconv.FormatFloat(currentUL, 'f', 1, 64), strconv.FormatFloat(maxUL, 'f', 1, 64), strconv.FormatFloat(avgUL, 'f', 1, 64))
			mc.rj.Render()
		case currentUL = <-mc.ulReport:
			ulReadingCount++
			ulReadingSum = ulReadingSum + currentUL
			avgUL = ulReadingSum / ulReadingCount
			if currentUL > maxUL {
				maxUL = currentUL
			}
			// Update our stats widget with the latest readings
			mc.rj.jobs["statsSummary"].(*termui.Par).Text = fmt.Sprintf("DOWNLOAD \nCurrent: %v Mbps\tMax: %v\tAvg: %v\n\nUPLOAD\nCurrent: %v Mbps\tMax: %v\tAvg: %v",
				strconv.FormatFloat(currentDL, 'f', 1, 64), strconv.FormatFloat(maxDL, 'f', 1, 64), strconv.FormatFloat(avgDL, 'f', 1, 64),
				strconv.FormatFloat(currentUL, 'f', 1, 64), strconv.FormatFloat(maxUL, 'f', 1, 64), strconv.FormatFloat(avgUL, 'f', 1, 64))
			mc.rj.Render()
		case <-mc.statsGeneratorDone:
			return
		}
	}
}

func (mc *meteredClient) updateProgressBar() {
	var updateIntervalMS uint = 500
	var progress uint

	mc.rj.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorRed

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
			mc.rj.jobs["progress"].(*termui.Gauge).Percent = int(progress)
			mc.rj.Render()
		case <-timer.C:
			mc.rj.jobs["progress"].(*termui.Gauge).Percent = 100
			mc.rj.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorGreen
			mc.rj.Render()
			return
		}
	}

}

func newRenderJobs() *renderJobs {
	rj := renderJobs{}
	rj.jobs = make(map[string]termui.Bufferer)
	return &rj
}

func (rj *renderJobs) Add(name string, job termui.Bufferer) {
	rj.jobs[name] = job
}

func (rj *renderJobs) Delete(name string) {
	delete(rj.jobs, name)
}

func (rj *renderJobs) Render() {
	var jobs []termui.Bufferer
	for _, j := range rj.jobs {
		jobs = append(jobs, j)
	}
	termui.Render(jobs...)
}
