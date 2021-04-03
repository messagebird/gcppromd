package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/messagebird/gcppromd"

	"bytes"
	"encoding/json"
	"io/ioutil"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	projectSeparator = ","
)

var (
	flisten           = flag.String("listen", ":8080", "HTTP listen address")
	fdaemon           = flag.Bool("daemon", false, "run the application as a daemon that periodically produces a target file with a json in Prometheus file_sd format. Disables web-mode")
	fouput            = flag.String("outputPath", "/etc/prom_sd/targets.json", "(daemon only)  A path to the output file with targets")
	fdiscovery        = flag.Int64("frequency", 300, "(daemon only)  discovery frequency in seconds")
	fprojects         = flag.String("projects", "", "(daemon only)  comma-separated projects IDs.")
	fprojectsauto     = flag.Bool("projects-auto-discovery", false, "(daemon only)  enable auto-discovery of the projects based on which projects can be listed by the provided credentials.")
	fprojectsexcludes = flag.String("projects-excludes", "", "(daemon only) RE2 regex, all projects matching it will not be discovered")
	fworkers          = flag.Int("workers", 20, "number of workers to perform the discovery")
)

func main() {
	flag.Parse()
	log.SetFormatter(&log.JSONFormatter{})

	idleConnsClosed := make(chan struct{})
	httpSrv := http.Server{Addr: *flisten, Handler: requestLogger(http.DefaultServeMux)}

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
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	gcpds, err := gcppromd.NewGCPProjectDiscovery()
	if *fprojectsauto && err != nil {
		log.WithError(err).Fatal("Cannot initialise GCP discovery")
	}

	if *fdaemon {
		var pexcludes *regexp.Regexp
		if *fprojectsexcludes != "" {
			pexcludes, err = regexp.Compile(*fprojectsexcludes)
			if err != nil {
				log.WithError(err).Fatal("Invalid project exclude pattern")
			}
		}

		log.Printf("Running as a daemon, output is going to be written to %s", *fouput)
		log.Printf("Targets update frequency: %v seconds", *fdiscovery)
		log.Printf("Projects Auto-Discovery: %t", *fprojectsauto)
		projects := parseProjectsSet(*fprojects)
		if len(projects) == 0 && !*fprojectsauto {
			log.Warnf("Empty '-projects=%s' flag in daemon mode", *fprojects)
		}
		log.Printf("Targets projects: %v", projectsSetList(projects))
		if pexcludes != nil {
			log.Printf("Projects exclude pattern: %s", pexcludes.String())
		}
		runDaemon(ctx, gceds, gcpds, DaemonConfig{
			Output:                 *fouput,
			Frequency:              time.Second * time.Duration(*fdiscovery),
			Projects:               projects,
			ProjectsExcludePattern: pexcludes,
			ProjectsAutoDiscovery:  *fprojectsauto,
		})
	} else {
		log.Printf("Running as a web-server")
		if *fprojects != "" {
			log.Warnf("Ignored '-projects=%s' flag in web-server mode", *fprojects)
		}
		if *fprojectsauto == true {
			log.Warnf("Ignored '-projects-auto-discovery=true' flag in web-server mode")
		}
		if *fprojectsexcludes != "" {
			log.Warnf("Ignored '-projects-excludes=%s' flag in web-server mode", *fprojectsexcludes)
		}
		runWebServer(ctx, gceds, gcpds, &httpSrv)
	}
	<-idleConnsClosed
}

type ProjectsSet map[string]interface{}

// parseProjects parse and de-duplicate a raw projects string project-1,project-b
func parseProjectsSet(raw string) (projects ProjectsSet) {
	projects = map[string]interface{}{}
	for _, project := range strings.Split(raw, projectSeparator) {
		if project == "" {
			continue
		}
		if _, has := projects[project]; !has {
			projects[project] = 1
		}
	}
	return
}

func projectsSetAdd(projects ProjectsSet, toAdd []string) ProjectsSet {
	for _, p := range toAdd {
		if p == "" {
			continue
		}
		if _, has := projects[p]; !has {
			projects[p] = 1
		}
	}
	return projects
}

func projectsSetList(set ProjectsSet) (out []string) {
	out = make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return
}

// projectSetExclude excludes all projects matching the given pattern
func projectsSetExclude(projects ProjectsSet, pattern *regexp.Regexp) ProjectsSet {
	if pattern == nil {
		return projects
	}
	out := make(map[string]interface{}, len(projects))
	for project := range projects {
		if !pattern.MatchString(project) {
			out[project] = 1
		}
	}
	return out
}

func collectTargets(ctx context.Context, gceds chan *gcppromd.GCEReqInstanceDiscovery, projects []string) ([]*gcppromd.PromConfig, bool) {
	if len(projects) == 0 {
		return []*gcppromd.PromConfig{}, true
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

// DaemonConfig configuration for the daemon
type DaemonConfig struct {
	Output                 string
	Frequency              time.Duration
	Projects               ProjectsSet
	ProjectsExcludePattern *regexp.Regexp
	ProjectsAutoDiscovery  bool
}

func runDaemon(
	ctx context.Context,
	gceds chan *gcppromd.GCEReqInstanceDiscovery,
	gcpds *gcppromd.GCPProjectDiscovery,
	cfg DaemonConfig,
) {
	timer := time.NewTimer(1 * time.Nanosecond)
	defer timer.Stop()

	projectsSet := projectsSetExclude(cfg.Projects, cfg.ProjectsExcludePattern)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(cfg.Frequency)

			discoveryStarted := time.Now()
			if cfg.ProjectsAutoDiscovery {
				discovered, err := gcpds.Projects(ctx)
				if err != nil {
					log.WithError(err).Error("can't auto-discover projects")
				} else {
					projectsSet = projectsSetExclude(projectsSetAdd(projectsSet, discovered), cfg.ProjectsExcludePattern)
				}
			}

			configs, ok := collectTargets(ctx, gceds, projectsSetList(projectsSet))
			if !ok {
				log.Info("invalid targets collection, skipping")
				continue
			}

			// write to a temporary file then swap it to ensure that the output file doesn't get corrupted or an half backed version is read.
			f, err := ioutil.TempFile(os.TempDir(), "")
			if err != nil {
				log.WithError(err).Error("can't open temporary file")
				continue
			}

			enc := json.NewEncoder(f)
			if err := enc.Encode(configs); err != nil {
				log.WithError(err).Error("can't encode prometheus targets configuration to json")
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
				err = os.Rename(f.Name(), cfg.Output)
			}
			if err != nil {
				log.WithError(err).WithField("file", cfg.Output).Error("could not write output file")
				os.Remove(f.Name())
				continue
			}

			log.Infof("target list updated, took %v", time.Since(discoveryStarted))
		}
	}
}

type handle struct {
	GCEDiscoveryWorkers chan *gcppromd.GCEReqInstanceDiscovery
	GCPProjectDiscovery *gcppromd.GCPProjectDiscovery
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

func runWebServer(ctx context.Context, gceds chan *gcppromd.GCEReqInstanceDiscovery, gcpds *gcppromd.GCPProjectDiscovery, srv *http.Server) {
	h := handle{gceds, gcpds}

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

	var err error

	// extracts a set of project names
	projectsSet := parseProjectsSet(r.URL.Query().Get("projects"))
	projectsExclude := r.URL.Query().Get("projects-excludes")
	projectsAutoDiscoveryValue := strings.ToLower(r.URL.Query().Get("projects-auto-discovery"))
	projectsAutoDiscovery := projectsAutoDiscoveryValue == "true" || projectsAutoDiscoveryValue == "1"

	var pexcludes *regexp.Regexp
	if projectsExclude != "" {
		pexcludes, err = regexp.Compile(projectsExclude)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if projectsAutoDiscovery {
		discovered, err := h.GCPProjectDiscovery.Projects(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		projectsSet = projectsSetAdd(projectsSet, discovered)
	}
	projectsSet = projectsSetExclude(projectsSet, pexcludes)

	configs, _ := collectTargets(r.Context(), h.GCEDiscoveryWorkers, projectsSetList(projectsSet))

	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(configs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(buf.Bytes())
	if err != nil {
		log.WithError(err).Error("unexpected error while witting response")
	}
}
