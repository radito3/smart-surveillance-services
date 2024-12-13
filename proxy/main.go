package main

import (
	"bytes"
	"context"
	"encoding/json"
	stdErrors "errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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
	NumItems int64        `json:"itemCount"`
	Pages    int64        `json:"pageCount"`
	Items    []PathConfig `json:"items"`
}

type PathConfig struct {
	Name   string `json:"name"`
	Source struct {
		Type string `json:"type"`
	} `json:"source"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

var ErrAlreadyExists = stdErrors.New("camera already exists")

var k8sClient *kubernetes.Clientset
var mtxSourceToPort map[string]string
var portToProtocol map[string]string

var hlsSubprocessPids map[string]*os.Process
var subprocessesLock sync.Mutex

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
	hlsSubprocessPids = make(map[string]*os.Process)
}

func addCamera(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	var data CameraEndpointRequest
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	resp, err := http.Get("http://localhost:9997/v3/paths/list")
	if err != nil {
		http.Error(writer, fmt.Sprintf("could not get paths list: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var pathsResp PathsListResponse
	err = json.NewDecoder(resp.Body).Decode(&pathsResp)
	if err != nil {
		http.Error(writer, fmt.Sprintf("could not read MediaMTX response: %v", err), http.StatusInternalServerError)
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
		if stdErrors.Is(err, ErrAlreadyExists) {
			http.Error(writer, err.Error(), http.StatusConflict)
			return
		}
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			port := parsedSourceURL.Port()
			if len(port) != 0 {
				port = ":" + parsedSourceURL.Port()
			}
			config.Source = parsedSourceURL.Scheme + "://" + getPodFQDN(hostname, currentInstanceIdx) + port + "/" + config.Path
			config.RunOnPublishRestart = false
			config.Record = false
			config.RunOnPublish = ""
			config.RunOnInit = ""

			err = sendConfigRequest(config, getPodFQDN(hostname, i))
			if err != nil {
				if stdErrors.Is(err, ErrAlreadyExists) {
					http.Error(writer, err.Error(), http.StatusConflict)
					return
				}
				// should we stop or continue here?
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	if parsedSourceURL.Scheme == "rtsp" || parsedSourceURL.Scheme == "rtmp" {
		go func() {
			time.Sleep(time.Second)
			if err = transcodeToHLS(parsedSourceURL.Scheme, config.Path); err != nil {
				log.Println(err)
			}
		}()
	}
	writer.WriteHeader(http.StatusOK)
}

func sendConfigRequest(config CameraEndpointConfig, host string) error {
	var buff bytes.Buffer
	json.NewEncoder(&buff).Encode(config)

	log.Println("Sending config request to " + host)
	res, err := http.Post("http://"+host+":9997/v3/config/paths/add/"+config.Path, "application/json", &buff)
	if err != nil {
		return fmt.Errorf("could not send config request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		var respErr ErrorResponse
		err := json.NewDecoder(res.Body).Decode(&respErr)
		if err != nil {
			return fmt.Errorf("could not read config response %d body: %v", res.StatusCode, err)
		}
		if respErr.Error == "path already exists" {
			return ErrAlreadyExists
		}
		return fmt.Errorf("config response error: %s", respErr.Error)
	}
	return nil
}

func getPodFQDN(hostname string, newSuffix int) string {
	podName := hostname[:strings.LastIndexByte(hostname, '-')]
	return fmt.Sprintf("%s-%d.mediamtx-headless.hub.svc.cluster.local", podName, newSuffix)
}

func getEndpoints(writer http.ResponseWriter, request *http.Request) {
	resp, err := http.Get("http://localhost:9997/v3/paths/list")
	if err != nil {
		http.Error(writer, fmt.Sprintf("could not get endpoints: %v", err), http.StatusInternalServerError)
		return
	}

	defer resp.Body.Close()

	_, err = io.CopyN(writer, resp.Body, resp.ContentLength)
	if err != nil {
		log.Printf("Could not send response to socket: %v\n", err)
	}
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
			Labels: map[string]string{
				"app": "analyser-deployment",
			},
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
								"http://notification-service.hub.svc.cluster.local:8081/alert/" + cameraID,
							},
							Env: []corev1.EnvVar{
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

	if value, present := os.LookupEnv("ACTIVITY_WHITELIST"); present {
		deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "ACTIVITY_WHITELIST", Value: value},
		)
	}

	deploymentsClient := k8sClient.AppsV1().Deployments("ml-analysis")
	// large timeout due to the size of the image
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, err := deploymentsClient.Create(ctx, deployment, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) || errors.IsConflict(err) {
		return err
	}
	if err != nil {
		return fmt.Errorf("failed to create Deployment: %v", err)
	}

	log.Println("Created Deployment ml-pipeline-" + cameraID)
	return nil
}

func int32Ptr(i int32) *int32 { return &i }

func deleteCameraEndpoint(writer http.ResponseWriter, request *http.Request) {
	cameraPath := chi.URLParam(request, "path")

	subprocessesLock.Lock()
	if process, present := hlsSubprocessPids[cameraPath]; present {
		process.Signal(os.Interrupt)
		delete(hlsSubprocessPids, cameraPath)
	}
	subprocessesLock.Unlock()

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
		var respErr ErrorResponse
		err := json.NewDecoder(resp.Body).Decode(&respErr)
		if err != nil {
			http.Error(writer, fmt.Sprintf("could not read config response body: %v", err), http.StatusInternalServerError)
			return
		}
		http.Error(writer, fmt.Sprintf("config response error: %s", respErr.Error), http.StatusInternalServerError)
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
			req, _ = http.NewRequest(http.MethodDelete, "http://"+getPodFQDN(hostname, i)+":9997/v3/config/paths/delete/"+cameraPath, nil)

			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusInternalServerError {
				var respErr ErrorResponse
				err := json.NewDecoder(resp.Body).Decode(&respErr)
				if err != nil {
					http.Error(writer, fmt.Sprintf("could not read config response body: %v", err), http.StatusInternalServerError)
					return
				}
				http.Error(writer, fmt.Sprintf("config response error: %s", respErr.Error), http.StatusInternalServerError)
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
	err = json.NewDecoder(resp.Body).Decode(&pathsResp)
	if err != nil {
		http.Error(writer, fmt.Sprintf("could not read MediaMTX response: %v", err), http.StatusInternalServerError)
		return
	}

	var port string
	found := false

	// paging?
	for _, item := range pathsResp.Items {
		if item.Name == "camera-"+cameraID {
			port = mtxSourceToPort[item.Source.Type]
			found = true
			break
		}
	}

	if !found {
		http.Error(writer, fmt.Sprintf("Camera with ID %s not found", cameraID), http.StatusNotFound)
		return
	}

	streamURL := fmt.Sprintf("%s://mediamtx.hub.svc.cluster.local:%s/camera-%s", portToProtocol[port], port, cameraID)

	err = createMlPipelineDeployment(streamURL, analysisMode, cameraID)
	if errors.IsAlreadyExists(err) || errors.IsConflict(err) {
		http.Error(writer, fmt.Sprintf("analysis for camera %s already started", cameraID), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	writer.WriteHeader(http.StatusCreated)
}

func stopAnalysis(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")

	deploymentsClient := k8sClient.AppsV1().Deployments("ml-analysis")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	// the default graceful termination timeout is 30s
	err := deploymentsClient.Delete(ctx, "ml-pipeline-"+cameraID, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		http.Error(writer, fmt.Sprintf("Deployment %s not found", "ml-pipeline-"+cameraID), http.StatusNotFound)
		return
	}
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
		if stdErrors.Is(err, ErrAlreadyExists) {
			http.Error(writer, err.Error(), http.StatusConflict)
			return
		}
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := 0; i < int(totalInstances); i++ {
		if i != currentInstanceIdx {
			config.Source = "rtsp://" + getPodFQDN(hostname, currentInstanceIdx) + ":8554/" + config.Path

			err = sendConfigRequest(config, getPodFQDN(hostname, i))
			if err != nil {
				if stdErrors.Is(err, ErrAlreadyExists) {
					http.Error(writer, err.Error(), http.StatusConflict)
					return
				}
				// should we stop or continue here?
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	job, err := createAnonymizationJob(hostname, streamPath)
	if errors.IsAlreadyExists(err) || errors.IsConflict(err) {
		http.Error(writer, fmt.Sprintf("anonymyzation for endpoint %s already started", streamPath), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	selector := metav1.FormatLabelSelector(job.Spec.Selector)
	log.Printf("Watching Pods for Job %s with selector %s...\n", "anonymization-filter-"+streamPath, selector)

	err = watchJobUntillRunning(request.Context(), selector, "anonymization-filter-"+streamPath)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	// this should not be here, as anonymisation is intended for recordings only
	go func() {
		// give the job some time to start publishing
		time.Sleep(5 * time.Second)
		if err = transcodeToHLS("rtsp", "anon-"+streamPath); err != nil {
			log.Println(err)
		}
	}()

	writer.WriteHeader(http.StatusOK)
}

func createAnonymizationJob(podHostname, streamPath string) (*batchv1.Job, error) {
	log.Println("Creating Job anonymization-filter-" + streamPath + " for Pod " + podHostname)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anonymization-filter-" + streamPath,
			Namespace: "hub",
			Labels: map[string]string{
				"app": "anonymization-filter-job",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
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
								"rtsp://mediamtx.hub.svc.cluster.local:8554/" + streamPath,
								"rtsp://" + podHostname + ".mediamtx-headless.hub.svc.cluster.local:8554/anon-" + streamPath,
							},
						},
					},
				},
			},
		},
	}

	jobsClient := k8sClient.BatchV1().Jobs("hub")
	// large timeout due to the size of the image
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resource, err := jobsClient.Create(ctx, job, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) || errors.IsConflict(err) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create Job: %v", err)
	}

	log.Println("Created Job anonymization-filter-" + streamPath)
	return resource, nil
}

func watchJobUntillRunning(ctx context.Context, selector, name string) error {
	podsClient := k8sClient.CoreV1().Pods("hub")
	watcher, err := podsClient.Watch(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("could not watch Job %s pods: %v", name, err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		switch event.Type {
		case watch.Added, watch.Modified:
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				return fmt.Errorf("unexpected object type in watch: %T", event.Object)
			}
			if pod.Status.Phase == corev1.PodRunning {
				log.Printf("Pod %s is running.\n", pod.Name)
				return nil
			} else {
				log.Printf("Pod %s status: %s\n", pod.Name, pod.Status.Phase)
			}
		case watch.Deleted:
			pod, ok := event.Object.(*corev1.Pod)
			if ok {
				log.Printf("Pod %s was deleted.\n", pod.Name)
			}
		case watch.Error:
			return fmt.Errorf("error watching pods: %v", err)
		}
	}
	return nil
}

func transcodeToHLS(protocol, streamPath string) error {
	var port string
	for mtxPort, supportedProtocol := range portToProtocol {
		if protocol == supportedProtocol {
			port = mtxPort
		}
	}
	if len(port) == 0 {
		return fmt.Errorf("unsupported protocol %s on path %s", protocol, streamPath)
	}
	streamURL := protocol + "://localhost:" + port + "/" + streamPath

	// https://trac.ffmpeg.org/wiki/StreamingGuide
	// https://trac.ffmpeg.org/wiki/Encode/H.264
	cmd := exec.Command("ffmpeg",
		"-i", streamURL,
		"-c:v", "libx264",
		"-an", // Disable audio
		"-tune", "zerolatency",
		"-preset", "veryfast",
		"-crf", "25", // h.264 constant rate factor (compression ratio) (default: 23)
		"-f", "hls",
		"-hls_time", "5", // seconds for each segment
		"-hls_list_size", "30", // number of segments to keep
		"-hls_flags", "delete_segments",
		"hls/"+streamPath+".m3u8")
	cmd.Stdout = log.Writer()
	cmd.Stderr = log.Writer()

	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("error transcoding video %s: %v", streamPath, err)
	}

	subprocessesLock.Lock()
	hlsSubprocessPids[streamPath] = cmd.Process
	subprocessesLock.Unlock()
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	log.Println("configuring server...")

	router := chi.NewRouter()

	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)
	router.Use(middleware.Heartbeat("/public/ping"))
	router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://*"},
		AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	router.Post("/endpoints", addCamera)
	router.Get("/endpoints", getEndpoints)
	router.Delete("/endpoints/{path}", deleteCameraEndpoint)
	router.Post("/analysis/{cameraID}", startAnalysis)
	router.Delete("/analysis/{cameraID}", stopAnalysis)
	router.Post("/anonymyze/{path}", anonymyze)

	workingDir, _ := os.Getwd()
	fs := http.Dir(filepath.Join(workingDir, "hls") + "/")

	router.Route("/static", func(r chi.Router) {
		r.Get("/*", http.StripPrefix("/static", http.FileServer(fs)).ServeHTTP)
	})

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
