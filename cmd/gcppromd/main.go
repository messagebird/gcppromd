package main

import (
	"context"
	"flag"
	"net/http"
	"strings"

	"github.com/messagebird/gcppromd"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	log "github.com/sirupsen/logrus"
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
	)
	flag.Parse()

	gceds, err := gcppromd.NewGCEDiscoveryPool(context.TODO(), gceWorkers)
	if err != nil {
		log.WithFields(log.Fields{
			"err":    err,
			"listen": *listenAddr,
		}).Fatal("Cannot initialise GCE discovery")
	}

	r := chi.NewRouter()

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
