package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/messagebird/gcppromd"

	"bytes"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"time"
)

const (
	projectSeparator   = ","
)

var (
	flisten    = flag.String("listen", ":8080", "HTTP listen address")
	fdaemon    = flag.Bool("daemon", false, "run the application as a daemon that periodically produces a target file with a json in Prometheus file_sd format. Disables web-mode")
	fouput     = flag.String("outputPath", "/etc/prom_sd/targets.json", "A path to the output file with targets")
	fdiscovery = flag.Int64("frequency", 300, "discovery frequency in seconds")
	fprojects  = flag.String("projects", "", "comma-separated projects IDs")
	fworkers   = flag.Int("workers", 20, "number of workers to perform the discovery")
)

func main() {
	flag.Parse()

	idleConnsClosed := make(chan struct{})
	httpSrv := http.Server{Addr:  *flisten, Handler: requestLogger(http.DefaultServeMux)}

	ctx, cancel := context.WithCancel(context.Background())
	ctxPool, cancelPool := context.WithCancel(context.Background())

	go func() {
		defer close(idleConnsClosed)
		defer cancelPool()
		defer cancel()

		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint
		log.Info("Received interrupt, shutting down")

		if !*fdaemon {
			ctx, cancel := context.WithTimeout(context.Background(), 60 * time.Second)
			defer cancel()
			if err := httpSrv.Shutdown(ctx); err != nil {
				// Error from closing listeners, or context timeout:
				log.Printf("HTTP server Shutdown: %v", err)
			}
		}
	}()


	gceds, err := gcppromd.NewGCEDiscoveryPool(ctxPool, *fworkers)

	if err != nil {
		log.WithError(err).Fatal("Cannot initialise GCE discovery")
	}

	if *fdaemon {
		log.Printf("Running as a daemon, output is going to be written to %s", *fouput)
		log.Printf("Targets update frequency: %v seconds", *fdiscovery)
		projects := parseProjects(*fprojects)
		if len(projects) == 0 {
			log.Warnf("Empty '-projects=%s' flag in daemon mode", *fprojects)
		}

		runDaemon(ctx, gceds, *fouput, time.Second*time.Duration(*fdiscovery), projects)
	} else {
		log.Printf("Running as a web-server")
		if *fprojects != "" {
			log.Warnf("Ignored '-projects=%s' flag in web-server mode", *fprojects)
		}

		runWebServer(ctx, gceds, &httpSrv)
	}
	<-idleConnsClosed
}

// parseProjects parse and de-duplicate a raw projects string project-1,project-b
func parseProjects(raw string) (projects []string) {
	seen := map[string]int{}
	for _, project := range strings.Split(raw, projectSeparator) {
		if project == "" {
			continue
		}
		seen[project]++
		if seen[project] <= 1 {
			projects = append(projects, project)
		}
	}
	return
}

func collectTargets(ctx context.Context, gceds chan *gcppromd.GCEReqInstanceDiscovery, projects []string) ([]*gcppromd.PromConfig, bool) {
	cerrors := make(chan error)
	defer close(cerrors)
	cconfigs := make(chan []*gcppromd.PromConfig)
	defer close(cconfigs)

	go func() {
		for _, project := range projects {
			gceds <- &gcppromd.GCEReqInstanceDiscovery{
				Project:           project,
				Filter:            "",
				PrometheusConfigs: cconfigs,
				Errors:            cerrors,
			}
		}
	}()

	configs := make([]*gcppromd.PromConfig, 0, 100)
	queries := len(projects)
	for n := 0; n < queries; n++ {
		select {
		case <-ctx.Done():
			return configs, false
		case err := <-cerrors:
			log.WithFields(log.Fields{
				"err": err,
			}).Println("Errors will discovering GCE instances.")
		case lconfigs := <-cconfigs:
			configs = append(configs, lconfigs...)
		}
	}

	return configs, true
}

func runDaemon(ctx context.Context, gceds chan *gcppromd.GCEReqInstanceDiscovery, output string, frequency time.Duration, projects []string) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			configs, ok := collectTargets(ctx, gceds, projects)
			if !ok {
				log.Info("invalid targets collection, skipping")
				continue
			}

			// write to a temporary file then swap it to ensure that the output file doesn't get corrupted or an half backed version is read.
			f, err := ioutil.TempFile(os.TempDir(), "")
			if err != nil {
				log.WithError(err).Error("Can't open temporary file")
				continue
			}

			enc := json.NewEncoder(f)
			if err := enc.Encode(configs); err != nil {
				log.WithError(err).Error("Can't encode prometheus targets configuration to json")
				continue
			}

			err = f.Sync()

			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if permErr := os.Chmod(f.Name(), 0644); err == nil {
				err = permErr
			}
			if err == nil {
				err = os.Rename(f.Name(), output)
			}
			if err != nil {
				log.WithError(err).WithField("file", output).Error("Could not write output file")
				os.Remove(f.Name())
				continue
			}

			log.Infof("Target list updated")
		}
	}
}

type handle struct {
	GCEDiscoveryWorkers chan *gcppromd.GCEReqInstanceDiscovery
}

func requestLogger(handler http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		t1 := time.Now()
		defer func() {
			log.WithFields(log.Fields{
				"method":   r.Method,
				"path":     r.URL,
				"duration": time.Since(t1),
			}).Println("")
		}()
		handler.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func runWebServer(ctx context.Context, gceds chan *gcppromd.GCEReqInstanceDiscovery, srv *http.Server) {
	h := handle{gceds}

	http.HandleFunc("/status", h.statusHandler)
	http.HandleFunc("/v1/gce/instances", h.instancesHandler)

	log.Infof("Listening on %s...", srv.Addr)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.WithError(err).Fatal("Failed to start HTTP daemon.")
	}
}

func (h *handle) statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("I'm fine."))
	if err != nil {
		log.WithError(err).Error("unexpected error while witting response")
	}
}

func (h *handle) instancesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "HEAD": // allowed methods
	default:
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	// extracts a set of project names
	projects := parseProjects(r.URL.Query().Get("projects"))

	configs, _ := collectTargets(context.Background(), h.GCEDiscoveryWorkers, projects)

	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(configs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(buf.Bytes())
	if err != nil {
		log.WithError(err).Error("unexpected error while witting response")
	}
}