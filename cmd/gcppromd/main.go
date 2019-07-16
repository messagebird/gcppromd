package main

import (
	"context"
	"flag"
	"net/http"
	"strings"

	"github.com/messagebird/gcppromd"

	"bytes"
	"encoding/json"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"time"
)

type ctxKey int

const (
	gcedsCtxKey ctxKey = iota
)

const (
	projectSeparator = ","
	gceWorkers       = 20
)

func main() {
	var (
		listenAddr = flag.String("listen", ":8080", "HTTP listen address")
		daemonMode = flag.Bool("daemon", false, "run the application as a daemon that periodically "+
			"produses a targets file with a json in Prometheus file_sd format. Disables web-mode")
		outputFile = flag.String("outputPath", "/etc/prom_sd/targets.json", "A path to the output"+
			"file with targets")
		discoveryFreq = flag.Int64("frequency", 300, "discovery frequency in seconds")
		projects      = flag.String("projects", "", "comma-separated projects IDs")
	)
	flag.Parse()

	gceds, err := gcppromd.NewGCEDiscoveryPool(context.TODO(), gceWorkers)
	if err != nil {
		log.WithFields(log.Fields{
			"err":    err,
			"listen": *listenAddr,
		}).Fatal("Cannot initialise GCE discovery")
	}

	if *daemonMode {
		log.Printf("Running as a daemon, output is going to be written to %s", *outputFile)
		log.Printf("Targets update frequency: %v seconds", *discoveryFreq)
		runDaemon(gceds, *outputFile, *discoveryFreq, *projects)
	}
	log.Printf("Running as a web-server")
	runWebServer(gceds, listenAddr)
}

func runDaemon(gceds chan *gcppromd.GCEReqInstanceDiscovery, output string, frequency int64, vprojects string) {

	ticker := time.NewTicker(time.Duration(frequency) * time.Second)
	for {
		select {
		case <-ticker.C:
			projects := []string{}
			seen := map[string]int{}
			for _, project := range strings.Split(vprojects, projectSeparator) {
				if project == "" {
					log.Println("Project list is empty.")
					continue
				}
				seen[project]++
				if seen[project] <= 1 {
					projects = append(projects, project)
				}
			}

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

			configs := []*gcppromd.PromConfig{}
			queries := len(projects)
			for n := 0; n < queries; n++ {
				select {
				case err := <-cerrors:
					log.WithFields(log.Fields{
						"err": err,
					}).Println("Errors will discovering GCE instances.")
				case lconfigs := <-cconfigs:
					configs = append(configs, lconfigs...)
				}
			}
			buf := &bytes.Buffer{}
			enc := json.NewEncoder(buf)
			if err := enc.Encode(configs); err != nil {
				log.Errorf("Can't decode json recieved from GCP")
				continue
			}
			if err := ioutil.WriteFile(output, buf.Bytes(), 0644); err != nil {
				log.Errorf("Could not write a file: ", err)
				continue
			}
			log.Infof("Target list updated")
		}

	}
}

func runWebServer(gceds chan *gcppromd.GCEReqInstanceDiscovery, listenAddr *string) {
	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			ctx = context.WithValue(ctx, gcedsCtxKey, gceds)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	r.Get("/status", statusHandler)
	r.Get("/v1/gce/instances", instancesHandler)

	log.Infof("Listening on %s...", *listenAddr)
	if err := http.ListenAndServe(*listenAddr, r); err != nil {
		log.WithFields(log.Fields{
			"err":    err,
			"listen": *listenAddr,
		}).Fatal("Failed to start HTTP daemon.")
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	render.PlainText(w, r, "I'm fine.")
}

func instancesHandler(w http.ResponseWriter, r *http.Request) {
	gced := r.Context().Value(gcedsCtxKey).(chan *gcppromd.GCEReqInstanceDiscovery)

	// extracts a set of project names
	vprojects := r.URL.Query().Get("projects")
	projects := []string{}
	seen := map[string]int{}
	for _, project := range strings.Split(vprojects, projectSeparator) {
		if project == "" {
			continue
		}
		seen[project]++
		if seen[project] <= 1 {
			projects = append(projects, project)
		}
	}

	cerrors := make(chan error)
	defer close(cerrors)
	cconfigs := make(chan []*gcppromd.PromConfig)
	defer close(cconfigs)

	go func() {
		for _, project := range projects {
			gced <- &gcppromd.GCEReqInstanceDiscovery{
				Project:           project,
				Filter:            "",
				PrometheusConfigs: cconfigs,
				Errors:            cerrors,
			}
		}
	}()

	configs := []*gcppromd.PromConfig{}
	queries := len(projects)
	for n := 0; n < queries; n++ {
		select {
		case <-r.Context().Done():
			log.Println("cancelled")
		case err := <-cerrors:
			log.WithFields(log.Fields{
				"err": err,
			}).Println("Errors will discovering GCE instances.")
		case lconfigs := <-cconfigs:
			configs = append(configs, lconfigs...)
		}
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, configs)
}
