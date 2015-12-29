package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gizak/termui"
)

const (
	blockSize        int64  = 10
	reportIntervalMS uint64 = 100 // report interval in milliseconds
)

type MeteredClient struct {
	blockTicker      chan bool
	throughputReport chan float64
	reporterDone     chan struct{}
	rendererDone     chan struct{}
	rj               *renderJobs
}

type renderJobs struct {
	jobs map[string]termui.Bufferer
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: ", os.Args[0], "<sparkyfish IP:port>")
	}

	err := termui.Init()
	if err != nil {
		panic(err)
	}
	defer termui.Close()

	mc := NewMeteredClient()

	mc.reporterDone = make(chan struct{})
	mc.rendererDone = make(chan struct{})

	mc.rj = NewRenderJobs()
	mc.createWidgets()

	termui.Handle("/sys/kbd/q", func(termui.Event) {
		termui.StopLoop()
	})

	go mc.ReportThroughput()
	go mc.MeasureThrougput()
	go mc.RenderDLReport()

	termui.Loop()

}

func (mc *MeteredClient) createWidgets() {
	dlGraph := termui.NewLineChart()
	dlGraph.BorderLabel = " Download Throughput "
	dlGraph.Width = 80
	dlGraph.Height = 12
	dlGraph.X = 0
	dlGraph.Y = 0
	//dlGraph.Mode = "dot"
	//dl.DotStyle = '+'
	dlGraph.AxesColor = termui.ColorWhite
	dlGraph.LineColor = termui.ColorGreen | termui.AttrBold

	dlReport := termui.NewPar("Borderless Text")
	dlReport.Height = 3
	dlReport.Width = 80
	dlReport.Y = 12
	dlReport.BorderLabel = " Download Test Report "

	helpBox := termui.NewPar("(q)uit")
	helpBox.Height = 1
	helpBox.Width = 80
	helpBox.Y = 16
	helpBox.Border = false

	mc.rj.Add("dlgraph", dlGraph)
	mc.rj.Add("dlreport", dlReport)
	mc.rj.Add("helpbox", helpBox)
}

// NewMeteredClient creates a new MeteredClient object
func NewMeteredClient() *MeteredClient {
	m := MeteredClient{}
	m.blockTicker = make(chan bool)
	m.throughputReport = make(chan float64)
	return &m
}

func (mc *MeteredClient) MeasureThrougput() {
	resp, err := http.Get(fmt.Sprint("http://", os.Args[1]))
	if err != nil {
		termui.Close()
		log.Fatalln(err)
	}

	for {
		_, err = io.CopyN(ioutil.Discard, resp.Body, 1024*blockSize)
		if err != nil {
			if err == io.EOF {
				close(mc.reporterDone)
				close(mc.rendererDone)
				return
			}
			log.Println("Error copying:", err)
			return
		}

		// // With each 100K copied, we send a message on our blockTicker channel
		mc.blockTicker <- true

	}
}

func (mc *MeteredClient) ReportThroughput() {
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

func (mc *MeteredClient) RenderDLReport() {
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

			mc.rj.jobs["dlgraph"].(*termui.LineChart).Data = readings
			mc.rj.jobs["dlreport"].(*termui.Par).Text = string(fmt.Sprint("Current: ", strconv.FormatFloat(r, 'f', 1, 64), " Mbps  Max: ", strconv.FormatFloat(maxThroughput, 'f', 1, 64), " Mbps  Avg: ", strconv.FormatFloat(avgThroughput, 'f', 1, 64), " Mbps"))
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
