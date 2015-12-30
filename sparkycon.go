package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/dustin/randbo"
	"github.com/gizak/termui"
)

const (
	blockSize        int64  = 10
	reportIntervalMS uint64 = 100 // report interval in milliseconds
	testLength       uint   = 15
)

// TestType is used to indicate the type of test being performed
type TestType int

const (
	Outbound TestType = iota
	Inbound
	Echo
)

type MeteredClient struct {
	blockTicker      chan bool
	throughputReport chan float64
	reporterDone     chan struct{}
	rendererDone     chan struct{}
	downloadTestDone chan struct{}
	rj               *renderJobs
}

type renderJobs struct {
	jobs map[string]termui.Bufferer
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: ", os.Args[0], " <sparkyfish IP:port>")
	}

	err := termui.Init()
	if err != nil {
		panic(err)
	}
	defer termui.Close()

	termui.Handle("/sys/kbd/q", func(termui.Event) {
		termui.StopLoop()
	})

	mc := NewMeteredClient()
	mc.rj = NewRenderJobs()

	go mc.runTestSequence()

	termui.Loop()

}

func (mc *MeteredClient) runTestSequence() {
	// First, we need to build the widgets on our screen.

	// Build a download graph widget
	dlGraph := termui.NewLineChart()
	dlGraph.BorderLabel = " Download Throughput "
	dlGraph.Width = 80
	dlGraph.Height = 12
	dlGraph.X = 0
	dlGraph.Y = 0
	dlGraph.Mode = "dot"
	dlGraph.DotStyle = '+'
	dlGraph.AxesColor = termui.ColorWhite
	dlGraph.LineColor = termui.ColorGreen | termui.AttrBold

	// Build a stats summary widget
	statsSummary := termui.NewPar("Borderless Text")
	statsSummary.Height = 3
	statsSummary.Width = 80
	statsSummary.Y = 12
	statsSummary.BorderLabel = " Tests Summary "

	// Build our helpbox
	helpBox := termui.NewPar("(q)uit")
	helpBox.Height = 1
	helpBox.Width = 80
	helpBox.Y = 16
	helpBox.Border = false

	mc.rj.Add("dlgraph", dlGraph)
	mc.rj.Add("statsSummary", statsSummary)
	mc.rj.Add("helpbox", helpBox)
	mc.rj.Render()

	// Run our download tests and block until that's done
	mc.runTest(Inbound)

	// Now we prepare the screen for the upload test.
	// Build a download graph widget
	ulGraph := termui.NewLineChart()
	ulGraph.BorderLabel = " Upload Throughput "
	ulGraph.Width = 80
	ulGraph.Height = 12
	ulGraph.X = 0
	ulGraph.Y = 0
	ulGraph.Mode = "dot"
	ulGraph.DotStyle = '+'
	ulGraph.AxesColor = termui.ColorWhite
	ulGraph.LineColor = termui.ColorRed | termui.AttrBold

	// Delete our download graph widget and replace it with our upload graph widget
	mc.rj.Delete("dlgraph")
	mc.rj.Add("ulgraph", ulGraph)
	mc.rj.Render()

	mc.runTest(Outbound)
	return

}

func (mc *MeteredClient) runTest(dir TestType) {
	mc.reporterDone = make(chan struct{})
	mc.rendererDone = make(chan struct{})

	go mc.MeasureThroughput()
	go mc.ReportThroughput(dir)
	mc.MeteredCopy(dir)
}

// NewMeteredClient creates a new MeteredClient object
func NewMeteredClient() *MeteredClient {
	m := MeteredClient{}
	m.blockTicker = make(chan bool)
	m.throughputReport = make(chan float64)
	return &m
}

func (mc *MeteredClient) MeteredCopy(dir TestType) {
	var rnd io.Reader
	var tl time.Duration

	conn, err := net.Dial("tcp", os.Args[1])
	if err != nil {
		termui.Close()
		log.Fatalln(err)
	}

	defer conn.Close()

	switch dir {
	case Inbound:
		// For inbound tests, we bump our timer by 2 seconds to account for
		// the remote server's test startup time
		tl = time.Second * time.Duration(testLength+2)
		_, err = conn.Write([]byte("SND"))
		if err != nil {
			termui.Close()
			log.Fatalln(err)
		}
	case Outbound:
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
			close(mc.reporterDone)
			close(mc.rendererDone)
			return
		default:
			switch dir {
			case Inbound:
				_, err = io.CopyN(ioutil.Discard, conn, 1024*blockSize)
			case Outbound:
				_, err = io.CopyN(conn, rnd, 1024*blockSize)
			}
			if err != nil {
				if err == io.EOF {
					close(mc.reporterDone)
					close(mc.rendererDone)
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

func (mc *MeteredClient) MeasureThroughput() {
	var blockCount, prevBlockCount uint64

	tick := time.NewTicker(time.Duration(reportIntervalMS) * time.Millisecond)
	for {
		select {
		case <-mc.blockTicker:
			// Increment our block counter when we get a ticker
			blockCount++
		case <-mc.reporterDone:
			tick.Stop()
			return
		case <-tick.C:
			// Every second, we calculate how many blocks were received
			// and derive an average throughput rate.
			//			r := float64((blockCount - prevBlockCount) * uint64(blockSize) * (1000 / reportIntervalMS))
			tr := (float64(blockCount - prevBlockCount)) * float64(blockSize) / float64(reportIntervalMS)
			mc.throughputReport <- tr
			prevBlockCount = blockCount
		}
	}
}

func (mc *MeteredClient) ReportThroughput(dir TestType) {
	var readings []float64
	var avgThroughput, maxThroughput float64

	var readingCount, readingSum float64

	for {
		select {
		case r := <-mc.throughputReport:
			readingCount++
			readingSum = readingSum + r
			avgThroughput = readingSum / readingCount

			if len(readings) >= 70 {
				readings = readings[1:]
			}

			if r > maxThroughput {
				maxThroughput = r
			}

			readings = append(readings, r)

			if dir == Inbound {
				mc.rj.jobs["dlgraph"].(*termui.LineChart).Data = readings
				mc.rj.jobs["statsSummary"].(*termui.Par).Text = string(fmt.Sprint("Current: ", strconv.FormatFloat(r, 'f', 1, 64), " Mbps  Max: ", strconv.FormatFloat(maxThroughput, 'f', 1, 64), " Mbps  Avg: ", strconv.FormatFloat(avgThroughput, 'f', 1, 64), " Mbps"))
			} else if dir == Outbound {
				mc.rj.jobs["ulgraph"].(*termui.LineChart).Data = readings
				mc.rj.jobs["statsSummary"].(*termui.Par).Text = string(fmt.Sprint("Current: ", strconv.FormatFloat(r, 'f', 1, 64), " Mbps  Max: ", strconv.FormatFloat(maxThroughput, 'f', 1, 64), " Mbps  Avg: ", strconv.FormatFloat(avgThroughput, 'f', 1, 64), " Mbps"))
			}
			mc.rj.Render()
		case <-mc.rendererDone:
			return
		}

	}
}

func NewRenderJobs() *renderJobs {
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
