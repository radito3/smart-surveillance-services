package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type CameraEndpoint struct {
	Name           string `json:"name"`
	Source         string
	MaxReaders     int64 `json:"maxReaders"`
	Fallback       string
	SourceOnDemand bool
}

type ReplicaSetStatus struct {
	Replicas int `json:"replicas"`
}

type ReplicaSet struct {
	Status ReplicaSetStatus `json:"status"`
}

func addCamera(w http.ResponseWriter, r *http.Request) {
	totalInstances, err := getReplicaSetInstances()
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}

	hostname, err := os.ReadFile("/etc/hostname")
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}

	ordinalIdx := string(hostname)[bytes.LastIndexByte(hostname, '-')+1:]

	// create a path config on the current instance
	_ = ordinalIdx
	cameraPayload := strings.NewReader("{\"a\":\"test\"}")
	res, err := http.Post("http://localhost:9997/v3/config/paths/add/{path}", "application/json", cameraPayload)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		w.WriteHeader(res.StatusCode)
		return
	}

	// TODO: create redirect paths in the rest of the instaces
	_ = totalInstances

	w.WriteHeader(200)
}

func getEndpoints(writer http.ResponseWriter, request *http.Request) {
	res, err := http.Get("http://localhost:9997/v3/paths")
	if err != nil {
		http.Error(writer, "server error", 500)
		return
	}

	defer res.Body.Close()

	_, err = io.ReadAll(res.Body)
	if err != nil {
		http.Error(writer, "server error", 500)
		return
	}

	writer.WriteHeader(200)
}

func getEndpointEncodings(writer http.ResponseWriter, request *http.Request) {
	path := chi.URLParam(request, "path")

	res, err := http.Get("http://localhost:9997/v3/paths/get/" + path)
	if err != nil {
		http.Error(writer, "server error", 500)
		return
	}

	defer res.Body.Close()

	writer.WriteHeader(200)
}

func getReplicaSetInstances() (int, error) {
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		// TODO: wrap all errors so we know where they come from
		return -1, err
	}

	caCertPath := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return -1, err
	}

	namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return -1, err
	}

	replicaSetName := "<replica-set-name>"

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: caCertPool,
		},
	}

	client := &http.Client{Transport: transport}

	// TODO: ensure the ServiceAccount has RBAC roles for getting and listing ReplcaSets
	apiURL := fmt.Sprintf(
		"https://kubernetes.default.svc/apis/apps/v1/namespaces/%s/replicasets/%s",
		string(namespace), replicaSetName,
	)

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", string(token)))

	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return -1, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return -1, err
	}

	var replicaSet ReplicaSet
	if err := json.Unmarshal(body, &replicaSet); err != nil {
		return -1, err
	}

	return replicaSet.Status.Replicas, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	log.Println("configuring server...")

	router := chi.NewRouter()

	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)
	router.Use(middleware.Heartbeat("/public/ping"))

	router.Post("/cameras", addCamera)
	router.Get("/cameras", getEndpoints)
	router.Get("/cameras/{path}", getEndpointEncodings)

	server := &http.Server{
		Addr:              ":8088",
		Handler:           router,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	server.SetKeepAlivesEnabled(false)

	go func() {
		log.Println("starting server...")
		// TODO: make an self-signed SSL certificate
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err.Error())
		}
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	<-signalChan

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()

	log.Println("shutting down server...")
	err := server.Shutdown(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}
}
