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
	testLength       uint   = 15
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

	f, err := os.OpenFile("sparkycon.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer f.Close()

	log.SetOutput(f)

	err = termui.Init()
	if err != nil {
		panic(err)
	}
	defer termui.Close()

	termui.Handle("/sys/kbd/q", func(termui.Event) {
		termui.StopLoop()
	})

	mc := newMeteredClient()
	mc.rj = newRenderJobs()

	go mc.runTestSequence()

	termui.Loop()

}

func (mc *meteredClient) runTestSequence() {
	// First, we need to build the widgets on our screen.

	// Build a download graph widget
	throughputGraph := termui.NewLineChart()
	throughputGraph.BorderLabel = " Download Throughput "
	throughputGraph.Width = 80
	throughputGraph.Height = 12
	throughputGraph.X = 0
	throughputGraph.Y = 0
	throughputGraph.Mode = "dot"
	throughputGraph.DotStyle = '+'
	throughputGraph.AxesColor = termui.ColorWhite
	throughputGraph.LineColor = termui.ColorGreen | termui.AttrBold

	// Build a stats summary widget
	statsSummary := termui.NewPar("Borderless Text")
	statsSummary.Height = 7
	statsSummary.Width = 80
	statsSummary.Y = 12
	statsSummary.BorderLabel = " Tests Summary "

	// Build our helpbox
	helpBox := termui.NewPar("(q)uit")
	helpBox.Height = 1
	helpBox.Width = 80
	helpBox.Y = 20
	helpBox.Border = false

	mc.rj.Add("throughputGraph", throughputGraph)
	mc.rj.Add("statsSummary", statsSummary)
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
	mc.runTest(inbound)

	close(mc.dlDone)

	mc.rj.Delete("throughputGraph")
	//mc.rj.Render()
	// Update our graph widget to indicate uplods
	throughputGraph = termui.NewLineChart()
	throughputGraph.BorderLabel = " Upload Throughput "
	throughputGraph.Width = 80
	throughputGraph.Height = 12
	throughputGraph.X = 0
	throughputGraph.Y = 0
	throughputGraph.Mode = "dot"
	throughputGraph.DotStyle = '+'
	throughputGraph.AxesColor = termui.ColorWhite
	throughputGraph.LineColor = termui.ColorRed | termui.AttrBold
	mc.rj.Add("throughputGraph", throughputGraph)
	mc.rj.Render()

	// Start our u/l throughput reporter to receive throughput measurements and send
	// them on to be rendered in our stats widget
	go mc.ReportThroughputUL()

	mc.runTest(outbound)

	close(mc.statsGeneratorDone)
	close(mc.ulDone)

	return
}

func (mc *meteredClient) runTest(dir testType) {
	measurerDone := make(chan struct{})

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

func (mc *meteredClient) MeteredCopy(dir testType, measurerDone chan<- struct{}) {
	var rnd io.Reader
	var tl time.Duration

	conn, err := net.Dial("tcp", os.Args[1])
	if err != nil {
		termui.Close()
		log.Fatalln(err)
	}

	defer conn.Close()

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

	for {
		select {
		case <-timer.C:
			// Timer has elapsed and test is finished
			close(measurerDone)
			return
		default:
			switch dir {
			case inbound:
				_, err = io.CopyN(ioutil.Discard, conn, 1024*blockSize)
			case outbound:
				_, err = io.CopyN(conn, rnd, 1024*blockSize)
			}
			if err != nil {
				if err == io.EOF {
					close(measurerDone)
					return
				}
				log.Println("Error copying:", err)
				return
			}
		}

		// // With each 100K copied, we send a message on our blockTicker channel
		mc.blockTicker <- true

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
