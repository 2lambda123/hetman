package cmd

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hainenber/hetman/internal/forwarder"
	"github.com/hainenber/hetman/internal/pipeline"
	"github.com/rs/zerolog/log"
)

type Aggregator struct {
	ConfigFile    string
	Port          int
	LogLevel      string
	agent         *Agent
	terminateChan chan os.Signal
}

// receiveLogPayload accepts log payload from upstreams and relay to next stage of agent's processing pipeline
func receiveLogPayload(inputChans []chan pipeline.Data) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "invalid method to submit logs.", http.StatusMethodNotAllowed)
			return
		}

		// Returns quickly if no request body found
		if r.Body == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Detect compressed body via "Content-Encoding" header
		// Unmarshal payload into proper struct
		// TODO: Converge to OpenTelemetry's log schema
		var (
			payload forwarder.Payload
			decoder *json.Decoder
		)
		switch r.Header.Get("Content-Encoding") {
		case "gzip":
			gr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "error handling compressed payload", http.StatusInternalServerError)
				return
			}
			defer gr.Close()

			// Read the decompressed data.
			decoder = json.NewDecoder(gr)
		default:
			decoder = json.NewDecoder(r.Body)
		}

		// Decode payload into struct
		if err := decoder.Decode(&payload); err != nil {
			http.Error(w, "error handling compressed payload", http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		// Send validated log payload to processing pipeline
		for _, stream := range payload.Streams {
			labels := stream.Stream
			for _, value := range stream.Values {
				for _, inputChan := range inputChans {
					inputChan <- pipeline.Data{
						Timestamp: value[0],
						LogLine:   value[1],
						Labels:    labels,
					}
				}
			}
		}
	}
}

func (a *Aggregator) Close() {
	if a.agent != nil {
		a.agent.Close()
	}
	if a.terminateChan != nil {
		a.terminateChan <- syscall.SIGTERM
	}
}

func (a *Aggregator) Run() {
	var (
		srv        = &http.Server{Addr: fmt.Sprintf(":%v", a.Port)}
		wg         sync.WaitGroup
		sleepCount int
	)

	a.agent = &Agent{
		ConfigFile: a.ConfigFile,
		LogLevel:   a.LogLevel,
	}
	a.terminateChan = make(chan os.Signal, 1)
	signal.Notify(a.terminateChan, os.Interrupt, syscall.SIGTERM)

	// Kickstart internal agent mode to run parallel with aggregator
	// For log processing
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.agent.Run()
	}()

	// Wait until orchestrator's components have finished instantiation of components
	// If not, shutdown internal agent and exit with non-zero err code
	for !a.agent.IsReady() {
		if sleepCount > 20 {
			a.agent.Orchestrator.Shutdown()
			wg.Wait()
			log.Fatal().Msg("Aggregator waiting too long for internal agent's initialization, 10 seconds have already elapsed! Please rerun the aggregator.")
		}
		time.Sleep(500 * time.Millisecond)
		sleepCount++
	}

	// Add a POST "/logs" route
	upstreamDataChans := a.agent.Orchestrator.GetUpstreamDataChans()
	http.HandleFunc("/logs", receiveLogPayload(upstreamDataChans))

	// A goroutine to gracefully close down HTTP server once signal received
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-a.terminateChan
		srv.Shutdown(context.Background())
	}()

	// Run a HTTP server
	srv.ListenAndServe()

	// Ensure log processing pipeline is properly closed and cleaned
	wg.Wait()

	log.Info().Msg("Aggregator server has been closed.")
}
