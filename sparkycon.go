package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

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
	statsGeneratorDone chan struct{}
	changeToUpload     chan struct{}
	wr                 *widgetRenderer
	rendererMu         *sync.Mutex
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
	go mc.runTestSequence()

	termui.Loop()
}

func (mc *meteredClient) runTestSequence() {
	// First, we need to build the widgets on our screen.

	// Build our title box
	titleBox := termui.NewPar("──────[ sparkyfish ]───────────────────────────────────────")
	titleBox.Height = 1
	titleBox.Width = 60
	titleBox.Y = 0
	titleBox.Border = false
	titleBox.TextFgColor = termui.ColorWhite | termui.AttrBold

	// Build a download graph widget
	dlGraph := termui.NewLineChart()
	dlGraph.BorderLabel = " Download Throughput "
	dlGraph.Width = 30
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
	ulGraph.Width = 30
	ulGraph.Height = 12
	ulGraph.X = 30
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
	statsSummary.Width = 60
	statsSummary.Y = 13
	statsSummary.BorderLabel = " Tests Summary "
	statsSummary.TextFgColor = termui.ColorWhite | termui.AttrBold

	// Build out progress gauge widget
	progress := termui.NewGauge()
	progress.Percent = 40
	progress.Width = 60
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
	helpBox.Width = 60
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

	// Prepare some channels that we'll use to signal
	// various state changes in the testing process
	mc.changeToUpload = make(chan struct{})
	mc.statsGeneratorDone = make(chan struct{})

	// Start our stats generator, which receives realtime measurements from the throughput
	// reporter and generates metrics from them
	go mc.generateStats()

	// Run our download tests and block until that's done
	mc.runThroughputTest(inbound)

	// Signal to our MeasureThroughput that we're about to begin the upload test
	close(mc.changeToUpload)

	// Run an outbound (upload) throughput test and block until it's complete
	mc.runThroughputTest(outbound)

	// Signal to our generators that the upload test is complete
	close(mc.statsGeneratorDone)

	return
}

// generateStats receives download and upload speed reports and computes metrics
// which are displayed in the stats widget.
func (mc *meteredClient) generateStats() {
	var measurement float64
	var currentDL, maxDL, avgDL float64
	var currentUL, maxUL, avgUL float64
	var dlReadingCount, dlReadingSum float64
	var ulReadingCount, ulReadingSum float64
	var dir = inbound

	for {
		select {
		case measurement = <-mc.throughputReport:
			switch dir {
			case inbound:
				currentDL = measurement
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
			case outbound:
				currentUL = measurement
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

			}
		case <-mc.changeToUpload:
			dir = outbound
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

	// Set a timer to advance the progress bar.  Since we test on a fixed
	// duration and not a fixed download size, we measure progress by time.
	timer := time.NewTimer(time.Second * time.Duration(testLength))

	for {
		select {
		case <-tick.C:
			progress = progress + progressPerUpdate
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = int(progress)
			mc.wr.Render()
		case <-timer.C:
			// Make sure that our progress bar always ends at 100%.  :)
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = 100
			mc.wr.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorGreen
			mc.wr.Render()
			return
		}
	}

}
