package web

import (
	"compress/gzip"
	"context"
	"strings"

	"github.com/goccy/go-json"

	"fmt"
	"io"
	"net/http"
	"runtime"

	"time"

	"github.com/amir20/dozzle/internal/docker"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/dustin/go-humanize"
	"github.com/go-chi/chi/v5"

	log "github.com/sirupsen/logrus"
)

func (h *handler) downloadLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	container, err := h.clientFromRequest(r).FindContainer(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()
	nowFmt := now.Format("2006-01-02T15-04-05")

	contentDisposition := fmt.Sprintf("attachment; filename=%s-%s.log", container.Name, nowFmt)

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Disposition", contentDisposition)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/text")
	} else {
		w.Header().Set("Content-Disposition", contentDisposition+".gz")
		w.Header().Set("Content-Type", "application/gzip")
	}

	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	zw := gzip.NewWriter(w)
	defer zw.Close()
	zw.Name = fmt.Sprintf("%s-%s.log", container.Name, nowFmt)
	zw.Comment = "Logs generated by Dozzle"
	zw.ModTime = now

	reader, err := h.clientFromRequest(r).ContainerLogsBetweenDates(r.Context(), id, time.Time{}, now, stdTypes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if container.Tty {
		io.Copy(zw, reader)
	} else {
		stdcopy.StdCopy(zw, zw, reader)
	}
}

func (h *handler) fetchLogsBetweenDates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-jsonl; charset=UTF-8")

	from, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
	to, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
	id := chi.URLParam(r, "id")

	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	container, err := h.clientFromRequest(r).FindContainer(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	reader, err := h.clientFromRequest(r).ContainerLogsBetweenDates(r.Context(), container.ID, from, to, stdTypes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	g := docker.NewEventGenerator(reader, container)
	encoder := json.NewEncoder(w)

	for event := range g.Events {
		if err := encoder.Encode(event); err != nil {
			log.Errorf("json encoding error while streaming %v", err.Error())
		}
	}
}

func (h *handler) newContainers(ctx context.Context) chan docker.Container {
	containers := make(chan docker.Container)
	for _, store := range h.stores {
		store.SubscribeNewContainers(ctx, containers)
	}

	return containers
}

func (h *handler) streamContainerLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	container, err := h.clientFromRequest(r).FindContainer(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	containers := make(chan docker.Container, 1)
	containers <- container

	go func() {
		newContainers := h.newContainers(r.Context())
		for {
			select {
			case container := <-newContainers:
				if container.ID == id {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamContainerLogs")
						return
					}
				}
			case <-r.Context().Done():
				log.Debugf("closing container channel streamContainerLogs")
				return
			}
		}
	}()

	streamLogsForContainers(w, r, h.clients, containers)
}

func (h *handler) streamLogsMerged(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("id") {
		http.Error(w, "ids query parameter is required", http.StatusBadRequest)
		return
	}

	containers := make(chan docker.Container, len(r.URL.Query()["id"]))

	for _, id := range r.URL.Query()["id"] {
		container, err := h.clientFromRequest(r).FindContainer(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		containers <- container
	}

	streamLogsForContainers(w, r, h.clients, containers)
}

func (h *handler) streamServiceLogs(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	containers := make(chan docker.Container, 10)

	go func() {
		for _, store := range h.stores {
			list, err := store.List()
			if err != nil {
				log.Errorf("error while listing containers %v", err.Error())
				return
			}

			for _, container := range list {
				if container.State == "running" && (container.Labels["com.docker.swarm.service.name"] == service) {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamServiceLogs")
						return
					}
				}
			}

		}
		newContainers := h.newContainers(r.Context())
		for {
			select {
			case container := <-newContainers:
				if container.State == "running" && (container.Labels["com.docker.swarm.service.name"] == service) {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamServiceLogs")
						return
					}
				}
			case <-r.Context().Done():
				log.Debugf("closing container channel streamServiceLogs")
				return
			}
		}
	}()

	streamLogsForContainers(w, r, h.clients, containers)
}

func (h *handler) streamGroupedLogs(w http.ResponseWriter, r *http.Request) {
	group := chi.URLParam(r, "group")
	containers := make(chan docker.Container, 10)

	go func() {
		for _, store := range h.stores {
			list, err := store.List()
			if err != nil {
				log.Errorf("error while listing containers %v", err.Error())
				return
			}

			for _, container := range list {
				if container.State == "running" && (container.Group == group) {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamServiceLogs")
						return
					}
				}
			}
		}
		newContainers := h.newContainers(r.Context())
		for {
			select {
			case container := <-newContainers:
				if container.State == "running" && (container.Group == group) {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamServiceLogs")
						return
					}
				}
			case <-r.Context().Done():
				log.Debugf("closing container channel streamServiceLogs")
				return
			}
		}
	}()

	streamLogsForContainers(w, r, h.clients, containers)
}

func (h *handler) streamStackLogs(w http.ResponseWriter, r *http.Request) {
	stack := chi.URLParam(r, "stack")
	containers := make(chan docker.Container, 10)

	go func() {
		for _, store := range h.stores {
			list, err := store.List()
			if err != nil {
				log.Errorf("error while listing containers %v", err.Error())
				return
			}

			for _, container := range list {
				if container.State == "running" && (container.Labels["com.docker.stack.namespace"] == stack) {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamStackLogs")
						return
					}
				}
			}
		}
		newContainers := h.newContainers(r.Context())
		for {
			select {
			case container := <-newContainers:
				if container.State == "running" && (container.Labels["com.docker.stack.namespace"] == stack) {
					select {
					case containers <- container:
					case <-r.Context().Done():
						log.Debugf("closing container channel streamStackLogs")
						return
					}
				}
			case <-r.Context().Done():
				log.Debugf("closing container channel streamStackLogs")
				return
			}
		}
	}()

	streamLogsForContainers(w, r, h.clients, containers)
}

func streamLogsForContainers(w http.ResponseWriter, r *http.Request, clients map[string]docker.Client, containers chan docker.Container) {
	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-transform")
	w.Header().Add("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	logs := make(chan *docker.LogEvent)
	events := make(chan *docker.ContainerEvent, 1)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	started := time.Now()

loop:
	for {
		select {
		case event := <-logs:
			if buf, err := json.Marshal(event); err != nil {
				log.Errorf("json encoding error while streaming %v", err.Error())
			} else {
				fmt.Fprintf(w, "data: %s\n", buf)
			}
			if event.Timestamp > 0 {
				fmt.Fprintf(w, "id: %d\n", event.Timestamp)
			}
			fmt.Fprintf(w, "\n")
			f.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ":ping \n\n")
			f.Flush()
		case container := <-containers:
			if container.StartedAt != nil && container.StartedAt.After(started) {
				events <- &docker.ContainerEvent{ActorID: container.ID, Name: "container-started", Host: container.Host}
			}
			go func(container docker.Container) {
				reader, err := clients[container.Host].ContainerLogs(r.Context(), container.ID, container.StartedAt, stdTypes)
				if err != nil {
					return
				}
				g := docker.NewEventGenerator(reader, container)
				for event := range g.Events {
					logs <- event
				}
				select {
				case err := <-g.Errors:
					if err != nil {
						if err == io.EOF {
							log.WithError(err).Debugf("stream closed for container %v", container.Name)
							events <- &docker.ContainerEvent{ActorID: container.ID, Name: "container-stopped", Host: container.Host}
						} else if err != r.Context().Err() {
							log.Errorf("unknown error while streaming %v", err.Error())
						}
					}
				default:
					// do nothing
				}
			}(container)

		case event := <-events:
			log.Debugf("received container event %v", event)
			if buf, err := json.Marshal(event); err != nil {
				log.Errorf("json encoding error while streaming %v", err.Error())
			} else {
				fmt.Fprintf(w, "event: container-event\ndata: %s\n\n", buf)
				f.Flush()
			}

		case <-r.Context().Done():
			log.Debugf("context cancelled")
			break loop
		}
	}

	if log.IsLevelEnabled(log.DebugLevel) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.WithFields(log.Fields{
			"allocated":      humanize.Bytes(m.Alloc),
			"totalAllocated": humanize.Bytes(m.TotalAlloc),
			"system":         humanize.Bytes(m.Sys),
			"routines":       runtime.NumGoroutine(),
		}).Debug("runtime mem stats")
	}
}
