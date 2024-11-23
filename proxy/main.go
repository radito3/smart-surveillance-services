package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type CameraEndpointRequest struct {
	ID                string `json:"ID"`
	Source            string `json:"source"`
	EnableTranscoding bool   `json:"enableTranscoding,omitempty"`
	Record            bool   `json:"record,omitempty"`
	MaxReaders        int64  `json:"maxReaders,omitempty"`
}

type CameraEndpointConfig struct {
	Path                       string `json:"name"`
	Source                     string `json:"source,omitempty"`
	MaxReaders                 int64  `json:"maxReaders,omitempty"`
	RunOnPublish               string `json:"runOnPublish,omitempty"`
	RunOnPublishRestart        bool   `json:"runOnPublishRestart,omitempty"`
	Record                     bool   `json:"record,omitempty"`
	RunOnInit                  string `json:"runOnInit,omitempty"`
	RunOnRecordSegmentComplete string `json:"runOnRecordSegmentComplete,omitempty"`
}

type PathsListResponse struct {
	Pages    int64        `json:"pageCount"`
	NumItems int64        `json:"itemCount"`
	Items    []PathConfig `json:"items"`
}

type PathConfig struct {
	Name   string `json:"name"`
	Source struct {
		Type string `json:"type"`
	} `json:"source"`
}

var k8sClient *kubernetes.Clientset
var mtxSourceToPort map[string]string
var portToProtocol map[string]string

func init() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to load in-cluster config: %v", err)
	}

	k8sClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create k8s client: %v", err)
	}

	mtxSourceToPort = map[string]string{
		"hlsSource":       "8888",
		"rpiCameraSource": "8554",
		"rtmpSource":      "1935",
		"rtmpConn":        "1935",
		"rtspSession":     "8554",
		"rtspSource":      "8554",
	}
	portToProtocol = map[string]string{
		"8554": "rtsp",
		"1935": "rtmp",
		"8888": "http",
	}
}

func addCamera(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	var data CameraEndpointRequest
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	parsedSourceURL, err := url.Parse(data.Source)
	if err != nil {
		http.Error(writer, fmt.Sprintf("Invalid Source URL %q: %v", data.Source, err), http.StatusBadRequest)
		return
	}

	totalInstances, err := getStatefulSetInstances()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	hostname := os.Getenv("HOSTNAME")

	currentInstanceIdxStr := hostname[strings.LastIndexByte(hostname, '-')+1:]
	currentInstanceIdx, _ := strconv.Atoi(currentInstanceIdxStr)

	var config CameraEndpointConfig
	config.Path = "camera-" + url.PathEscape(data.ID)
	config.MaxReaders = data.MaxReaders
	config.Source = data.Source
	if data.EnableTranscoding {
		config.RunOnPublishRestart = true
		config.RunOnPublish = "ffmpeg -i rtsp://localhost:8554/" + config.Path + " -map 0:v -map 0:a " +
			"-b:v:0 500k -maxrate:v:0 500k -bufsize:v:0 1000k -s:v:0 640x360 -c:v:0 libx264 " +
			"-b:v:1 1000k -maxrate:v:1 1000k -bufsize:v:1 2000k -s:v:1 1280x720 -c:v:1 libx264 " +
			"-b:v:2 2000k -maxrate:v:2 2000k -bufsize:v:2 4000k -s:v:2 1920x1080 -c:v:2 libx264 " +
			"-c:a aac -b:a 128k -f hls -hls_time 4 -hls_list_size 5 -hls_flags delete_segments " +
			"-var_stream_map \"v:0,a:0 v:1,a:1 v:2,a:2\" " +
			"-master_pl_name master.m3u8 " +
			"-hls_segment_filename /" + config.Path + "/stream_%v/segment_%d.ts " +
			"/" + config.Path + "/stream_%v/playlist.m3u8"
	}
	if data.Record {
		// TODO: run rclone config with the user-provided data
		config.Record = true
		// sync the whole directory in case a crash has occured
		config.RunOnInit = "rclone sync /recordings myconfig:/recordings"
		config.RunOnRecordSegmentComplete = "rclone sync --min-age=5s /recordings/" + config.Path + " myconfig:/recordings/" + config.Path
	}

	err = sendConfigRequest(config, "localhost")
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			port := parsedSourceURL.Port()
			if len(port) != 0 {
				port = ":" + parsedSourceURL.Port()
			}
			config.Source = parsedSourceURL.Scheme + "://" + getNewHostname(hostname, currentInstanceIdx) + port + "/" + config.Path
			config.RunOnPublishRestart = false
			config.Record = false
			config.RunOnPublish = ""
			config.RunOnInit = ""

			err = sendConfigRequest(config, getNewHostname(hostname, i))
			if err != nil {
				// should we stop or continue here?
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	writer.WriteHeader(http.StatusOK)
}

func sendConfigRequest(config CameraEndpointConfig, host string) error {
	cameraPayload, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("could not serialize config request: %v", err)
	}

	res, err := http.Post("http://"+host+":9997/v3/config/paths/add/"+config.Path, "application/json", bytes.NewReader(cameraPayload))
	if err != nil {
		return fmt.Errorf("could not send config request: %v", err)
	}
	defer res.Body.Close()

	// TODO: handle the case where the camera endpoint is already created
	if res.StatusCode/100 != 2 {
		bodyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("could not read config response body: %v", err)
		}
		return fmt.Errorf("config response error: %s", string(bodyBytes))
	}
	return nil
}

func getNewHostname(hostname string, newSuffix int) string {
	podName := hostname[:strings.LastIndexByte(hostname, '-')]
	return fmt.Sprintf("%s-%d.mediamtx-headless.hub.svc.cluster.local", podName, newSuffix)
}

func getEndpoints(writer http.ResponseWriter, request *http.Request) {
	// this will not be the full config, just a summary
	addr, _ := url.ParseRequestURI("http://localhost:9997/v3/paths/list")

	proxy := httputil.NewSingleHostReverseProxy(addr)

	proxy.ServeHTTP(writer, request)
}

func getStatefulSetInstances() (int32, error) {
	statefulSetsClient := k8sClient.AppsV1().StatefulSets("hub")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	log.Println("Checking StatefulSet instances...")
	sts, err := statefulSetsClient.Get(ctx, "mediamtx", metav1.GetOptions{})
	if err != nil {
		return -1, fmt.Errorf("could not get StatefulSet mediamtx: %v", err)
	}

	return sts.Status.Replicas, nil
}

func createMlPipelineDeployment(streamURL, analysisMode, cameraID string) error {
	log.Println("Creating Deployment ml-pipeline-" + cameraID)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ml-pipeline-" + cameraID,
			Namespace: "ml-analysis",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "analyser",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "analyser",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "ml-pipeline",
							Image: "radito3/ss-ml-analysis:1.0.0",
							Args: []string{
								streamURL,
								analysisMode,
								"http://notification-service.hub.svc.cluster.local/alert/" + cameraID,
							},
							Env: []corev1.EnvVar{
								{
									Name:  "ACTIVITY_WHITELIST",
									Value: "1-30",
								},
								{
									Name:  "PYTHONUNBUFFERED",
									Value: "1",
								},
								{
									Name:  "LOG_LEVEL",
									Value: "DEBUG",
								},
							},
						},
					},
				},
			},
		},
	}

	deplomentsClient := k8sClient.AppsV1().Deployments("ml-analysis")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := deplomentsClient.Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create Deployment: %v", err)
	}

	log.Println("Created Deployment ml-pipeline-" + cameraID)
	return nil
}

func int32Ptr(i int32) *int32 { return &i }

func deleteCameraEndpoint(writer http.ResponseWriter, request *http.Request) {
	cameraPath := chi.URLParam(request, "path")

	req, _ := http.NewRequest(http.MethodDelete, "http://localhost:9997/v3/config/paths/delete/"+cameraPath, nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		http.Error(writer, fmt.Sprintf("camera path %q not found", cameraPath), http.StatusNotFound)
		return
	}

	if resp.StatusCode == http.StatusInternalServerError {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(writer, fmt.Sprintf("could not read config response body: %v", err), http.StatusInternalServerError)
			return
		}
		http.Error(writer, fmt.Sprintf("config response error: %s", string(bodyBytes)), http.StatusInternalServerError)
		return
	}

	totalInstances, err := getStatefulSetInstances()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	hostname := os.Getenv("HOSTNAME")

	currentInstanceIdxStr := hostname[strings.LastIndexByte(hostname, '-')+1:]
	currentInstanceIdx, _ := strconv.Atoi(currentInstanceIdxStr)

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			req, _ = http.NewRequest(http.MethodDelete, "http://"+getNewHostname(hostname, i)+":9997/v3/config/paths/delete/"+cameraPath, nil)

			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusInternalServerError {
				bodyBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					http.Error(writer, fmt.Sprintf("could not read config response body: %v", err), http.StatusInternalServerError)
					return
				}
				http.Error(writer, fmt.Sprintf("config response error: %s", string(bodyBytes)), http.StatusInternalServerError)
				return
			}
		}
	}

	writer.WriteHeader(http.StatusNoContent)
}

func startAnalysis(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")
	analysisMode := request.URL.Query().Get("analysisMode")

	resp, err := http.Get("http://localhost:9997/v3/paths/list")
	if err != nil {
		http.Error(writer, fmt.Sprintf("could not get paths list: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var pathsResp PathsListResponse
	// ignore any parsing errors, because we assume MediaMTX returns valid responses
	json.NewDecoder(resp.Body).Decode(&pathsResp)

	streamURL := "http://mediamtx.hub.svc.cluster.local:"
	found := false

	// paging?
	for _, item := range pathsResp.Items {
		if item.Name == "camera-"+cameraID {
			streamURL += mtxSourceToPort[item.Source.Type] + "/camera-" + cameraID
			found = true
			break
		}
	}

	if !found {
		http.Error(writer, fmt.Sprintf("Camera with ID %s not found", cameraID), http.StatusNotFound)
		return
	}

	err = createMlPipelineDeployment(streamURL, analysisMode, cameraID)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	writer.WriteHeader(http.StatusCreated)
}

func stopAnalysis(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")

	deplomentsClient := k8sClient.AppsV1().Deployments("ml-analysis")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	// the default graceful termination timeout is 30s
	err := deplomentsClient.Delete(ctx, "ml-pipeline-"+cameraID, metav1.DeleteOptions{})
	if err != nil {
		http.Error(writer, fmt.Sprintf("failed to delete Deployment: %v", err), http.StatusInternalServerError)
		return
	}

	writer.WriteHeader(http.StatusOK)
}

func anonymyze(writer http.ResponseWriter, request *http.Request) {
	streamPath := chi.URLParam(request, "path")

	totalInstances, err := getStatefulSetInstances()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	hostname := os.Getenv("HOSTNAME")

	currentInstanceIdxStr := hostname[strings.LastIndexByte(hostname, '-')+1:]
	currentInstanceIdx, _ := strconv.Atoi(currentInstanceIdxStr)

	var config CameraEndpointConfig
	config.Path = "anon-" + streamPath
	config.Source = "publisher"

	err = sendConfigRequest(config, "localhost")
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			config.Source = "rtsp://" + getNewHostname(hostname, currentInstanceIdx) + ":8554/" + config.Path

			err = sendConfigRequest(config, getNewHostname(hostname, i))
			if err != nil {
				// should we stop or continue here?
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	err = createAnonymizationPod(hostname, streamPath)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	writer.WriteHeader(http.StatusOK)
}

func createAnonymizationPod(podHostname, streamPath string) error {
	log.Println("Creating Pod anonymization-filter-" + streamPath)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anonymization-filter-" + streamPath,
			Namespace: "hub",
			Labels: map[string]string{
				"app": "anonymization-filter",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers: []corev1.Container{
				{
					Name:  "anonymization-filter",
					Image: "radito3/ss-anonymisation-filter:1.0.0",
					Args: []string{
						"rtsp://mediamtx.hub.svc.cluster.local/" + streamPath,
						"rtsp://" + podHostname + ".mediamtx-headless.hub.svc.cluster.local/anon-" + streamPath,
					},
				},
			},
		},
	}

	podsClient := k8sClient.CoreV1().Pods("hub")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := podsClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create Pod: %v", err)
	}

	log.Println("Created Pod anonymization-filter-" + streamPath)
	return nil
}

func transcodeToMPEGTS(streamURL string, wsConn *websocket.Conn) {
	// https://trac.ffmpeg.org/wiki/StreamingGuide
	cmd := exec.Command("ffmpeg", "-re", "-i", streamURL, "-f", "mpegts", "-codec:v", "mpeg1video", "-")
	// for HLS:
	// ffmpeg -re -i $streamURL -c:v libx264 -preset veryfast -crf 23 -f hls -hls_time 2 -hls_list_size 5 -hls_flags delete_segments output.m3u8

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("Error creating stdout pipe:", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Println("Error starting FFmpeg:", err)
		return
	}
	defer cmd.Process.Signal(os.Interrupt)

	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			log.Println("Error reading from FFmpeg stdout:", err)
			break
		}

		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			log.Println("Error sending WebSocket message:", err)
			break
		}
	}
}

func wsHandler(writer http.ResponseWriter, request *http.Request) {
	streamPath := chi.URLParam(request, "path")

	resp, err := http.Get("http://localhost:9997/v3/paths/list")
	if err != nil {
		http.Error(writer, fmt.Sprintf("could not get paths list: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var pathsResp PathsListResponse
	// ignore any parsing errors, because we assume MediaMTX returns valid responses
	json.NewDecoder(resp.Body).Decode(&pathsResp)

	var port string
	var sourceType string
	found := false

	// paging?
	for _, item := range pathsResp.Items {
		if item.Name == streamPath {
			sourceType = item.Source.Type
			port = mtxSourceToPort[item.Source.Type]
			found = true
			break
		}
	}

	if !found {
		http.Error(writer, fmt.Sprintf("Stream path %s not found", streamPath), http.StatusNotFound)
		return
	}

	streamURL := portToProtocol[port] + "://localhost:" + mtxSourceToPort[sourceType] + "/" + streamPath

	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all connections
		},
	}
	conn, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer conn.Close()

	log.Printf("Client connected to stream path: %s", streamPath)
	transcodeToMPEGTS(streamURL, conn)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	log.Println("configuring server...")

	router := chi.NewRouter()

	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)
	router.Use(middleware.Heartbeat("/public/ping"))

	router.Post("/endpoints", addCamera)
	router.Get("/endpoints", getEndpoints)
	router.Delete("/endpoints/{path}", deleteCameraEndpoint)
	router.Post("/analysis/{cameraID}", startAnalysis)
	router.Delete("/analysis/{cameraID}", stopAnalysis)
	router.Post("/anonymyze/{path}", anonymyze)
	router.Get("/ws/{path}", wsHandler)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           router,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	go func() {
		log.Println("starting server...")
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
