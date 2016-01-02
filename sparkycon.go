package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gizak/termui"
)

const (
	blockSize            int64  = 50  // size (KB) of each block of random data to be sent/rec'd
	reportIntervalMS     uint64 = 300 // report interval in milliseconds
	throughputTestLength uint   = 10  // length of time to conduct each throughput test
	maxPingTestLength    uint   = 10  // maximum time for ping test to complete
	numPings             int    = 30  // number of pings to attempt
)

// testType is used to indicate the type of test being performed
type testType int

const (
	outbound testType = iota // upload test
	inbound                  // download test
	echo                     // echo (ping) test
)

type meteredClient struct {
	pingTime           chan time.Duration
	blockTicker        chan bool
	pingProgressTicker chan bool
	testDone           chan bool
	allTestsDone       chan struct{}
	progressBarReset   chan bool
	throughputReport   chan float64
	statsGeneratorDone chan struct{}
	changeToUpload     chan struct{}
	pingProcessorReady chan struct{}
	wr                 *widgetRenderer
	rendererMu         *sync.Mutex
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: ", os.Args[0], " <sparkyfish IP:port>")
	}

	// logf, err := os.OpenFile("sparkyfish.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	// if err != nil {
	// 	log.Fatalf("error opening file: %v", err)
	// }
	// defer logf.Close()
	//
	// log.SetOutput(logf)

	// Initialize our screen
	err := termui.Init()
	if err != nil {
		panic(err)
	}

	if termui.TermWidth() < 60 || termui.TermHeight() < 28 {
		fmt.Println("sparkyfish needs a terminal window at least 60x28 to run.")
		os.Exit(1)
	}

	defer termui.Close()

	// 'q' quits the program
	termui.Handle("/sys/kbd/q", func(termui.Event) {
		termui.StopLoop()
	})
	// 'Q' also works
	termui.Handle("/sys/kbd/Q", func(termui.Event) {
		termui.StopLoop()
	})

	mc := newMeteredClient()
	mc.prepareChannels()

	mc.wr = newwidgetRenderer()

	// Begin our tests
	go mc.runTestSequence()

	termui.Loop()
}

// NewMeteredClient creates a new MeteredClient object
func newMeteredClient() *meteredClient {
	m := meteredClient{}
	return &m
}

func (mc *meteredClient) prepareChannels() {

	// Prepare some channels that we'll use for measuring
	// throughput and latency
	mc.blockTicker = make(chan bool)
	mc.throughputReport = make(chan float64)
	mc.pingTime = make(chan time.Duration, 10)
	mc.pingProgressTicker = make(chan bool, numPings)

	// Prepare some channels that we'll use to signal
	// various state changes in the testing process
	mc.pingProcessorReady = make(chan struct{})
	mc.changeToUpload = make(chan struct{})
	mc.statsGeneratorDone = make(chan struct{})
	mc.testDone = make(chan bool)
	mc.progressBarReset = make(chan bool)
	mc.allTestsDone = make(chan struct{})

}

func (mc *meteredClient) runTestSequence() {
	// First, we need to build the widgets on our screen.

	// Build our title box
	titleBox := termui.NewPar("──────[ sparkyfish ]────────────────────────────────────────")
	titleBox.Height = 1
	titleBox.Width = 60
	titleBox.Y = 0
	titleBox.Border = false
	titleBox.TextFgColor = termui.ColorWhite | termui.AttrBold

	// Build a download graph widget
	dlGraph := termui.NewLineChart()
	dlGraph.BorderLabel = " Download Throughput "
	dlGraph.Data = []float64{0}
	dlGraph.Width = 30
	dlGraph.Height = 12
	dlGraph.PaddingTop = 1
	dlGraph.X = 0
	dlGraph.Y = 5
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
	ulGraph.PaddingTop = 1
	ulGraph.X = 30
	ulGraph.Y = 5
	// Windows Command Prompt doesn't support our Unicode characters with the default font
	if runtime.GOOS == "windows" {
		ulGraph.Mode = "dot"
		ulGraph.DotStyle = '+'
	}
	ulGraph.AxesColor = termui.ColorWhite
	ulGraph.LineColor = termui.ColorGreen | termui.AttrBold

	latencyGraph := termui.NewSparkline()
	latencyGraph.LineColor = termui.ColorCyan
	latencyGraph.Height = 3

	latencyGroup := termui.NewSparklines(latencyGraph)
	latencyGroup.Y = 2
	latencyGroup.Height = 3
	latencyGroup.Width = 30
	latencyGroup.Border = false
	latencyGroup.Lines[0].Data = []int{0}

	latencyTitle := termui.NewPar("Latency")
	latencyTitle.Height = 1
	latencyTitle.Width = 30
	latencyTitle.Border = false
	latencyTitle.TextFgColor = termui.ColorGreen
	latencyTitle.Y = 1

	latencyStats := termui.NewPar("")
	latencyStats.Height = 4
	latencyStats.Width = 30
	latencyStats.X = 32
	latencyStats.Y = 1
	latencyStats.Border = false
	latencyStats.TextFgColor = termui.ColorWhite | termui.AttrBold
	latencyStats.Text = "Last: 30ms\nMin: 2ms\nMax: 34ms"

	// Build a stats summary widget
	statsSummary := termui.NewPar("")
	statsSummary.Height = 7
	statsSummary.Width = 60
	statsSummary.Y = 17
	statsSummary.BorderLabel = " Throughput Summary "
	statsSummary.Text = fmt.Sprintf("DOWNLOAD \nCurrent: -- Mbps\tMax: --\tAvg: --\n\nUPLOAD\nCurrent: -- Mbps\tMax: --\tAvg: --")
	statsSummary.TextFgColor = termui.ColorWhite | termui.AttrBold

	// Build out progress gauge widget
	progress := termui.NewGauge()
	progress.Percent = 40
	progress.Width = 60
	progress.Height = 3
	progress.Y = 24
	progress.X = 0
	progress.Border = true
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
	helpBox.Y = 27
	helpBox.Border = false
	helpBox.TextBgColor = termui.ColorBlue
	helpBox.TextFgColor = termui.ColorYellow | termui.AttrBold
	helpBox.Bg = termui.ColorBlue

	// Add the widgets to the rendering jobs and render the screen
	mc.wr.Add("titlebox", titleBox)
	mc.wr.Add("dlgraph", dlGraph)
	mc.wr.Add("ulgraph", ulGraph)
	mc.wr.Add("latency", latencyGroup)
	mc.wr.Add("latencytitle", latencyTitle)
	mc.wr.Add("latencystats", latencyStats)
	mc.wr.Add("statsSummary", statsSummary)
	mc.wr.Add("progress", progress)
	mc.wr.Add("helpbox", helpBox)
	mc.wr.Render()

	// Launch a progress bar updater
	go mc.updateProgressBar()

	// Start our ping test and block until it's complete
	mc.pingTest()

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

	// Notify the progress bar updater to change the bar color to green
	close(mc.allTestsDone)

	return
}

// updateProgressBar updates the progress bar as tests run
func (mc *meteredClient) updateProgressBar() {
	var updateIntervalMS uint = 500
	var progress uint

	mc.wr.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorRed

	//progressPerUpdate := throughputTestLength / (updateIntervalMS / 1000)
	var progressPerUpdate uint = 100 / 20

	// Set a ticker for advancing the progress bar
	tick := time.NewTicker(time.Duration(updateIntervalMS) * time.Millisecond)

	for {
		select {
		case <-tick.C:
			// Update via our update interval ticker, but never beyond 100%
			progress = progress + progressPerUpdate
			if progress > 100 {
				progress = 100
			}
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = int(progress)
			mc.wr.Render()

		case <-mc.pingProgressTicker:
			// Update as each ping comes back, but never beyond 100%
			progress = progress + uint(100/numPings)
			if progress > 100 {
				progress = 100
			}
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = int(progress)
			mc.wr.Render()

			// No need to render, since it's already happening with each ping
		case <-mc.testDone:
			// As each test completes, we set the progress bar to 100% completion.
			// It will be reset to 0% at the start of the next test.
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = 100
			mc.wr.Render()
		case <-mc.progressBarReset:
			// Reset our progress tracker
			progress = 0
			// Reset the progress bar
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = 0
			mc.wr.Render()
		case <-mc.allTestsDone:
			// Make sure that our progress bar always ends at 100%.  :)
			mc.wr.jobs["progress"].(*termui.Gauge).Percent = 100
			mc.wr.jobs["progress"].(*termui.Gauge).BarColor = termui.ColorGreen
			mc.wr.Render()
			return
		}
	}

}
