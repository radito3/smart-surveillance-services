package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

type RequestData struct {
	CameraID  string           `json:"camera_id,omitempty"`
	Receivers []ReceiverConfig `json:"receivers,omitempty"`
}

type ReceiverConfig struct {
	UIPopup       bool        `json:"ui_popup,omitempty"` // default
	eventsChannel chan string `json:"-"`
	connOpen      bool        `json:"-"`

	WebhookURL         string `json:"webhook_url,omitempty"`
	WebhookTimeout     int64  `json:"webhook_timeout,omitempty"`     // default: 10s
	WebhookAuthType    string `json:"webhook_auth_type,omitempty"`   // default: basic
	WebhookCredentials string `json:"webhook_credentials,omitempty"` // encrypted

	SmtpServer      string `json:"smtp_server,omitempty"`
	SmtpSender      string `json:"smtp_sender,omitempty"`
	SmtpRecipient   string `json:"smtp_recipient,omitempty"`
	SmtpCredentials string `json:"smtp_credentials,omitempty"` // encrypted
}

var receivers map[string][]ReceiverConfig = make(map[string][]ReceiverConfig)
var privateKey *rsa.PrivateKey

func init() {
	privateKeyData, err := os.ReadFile("/etc/private-key/private_key.pem")
	if err != nil {
		log.Fatalf("failed to load private key: %v", err)
	}

	block, _ := pem.Decode(privateKeyData)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		log.Fatalf("failed to parse private key: %v", err)
	}
	privateKey = key
}

func addCameraConfig(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	var data RequestData
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(data.CameraID) == 0 {
		http.Error(writer, "Missing required Camera ID", http.StatusBadRequest)
		return
	}

	if len(data.Receivers) == 0 {
		http.Error(writer, "No receivers", http.StatusBadRequest)
		return
	}

	_, present := receivers[data.CameraID]
	if present {
		http.Error(writer, fmt.Sprintf("Camera %s already configured", data.CameraID), http.StatusConflict)
		return
	}

	receivers[data.CameraID] = data.Receivers
	writer.WriteHeader(http.StatusCreated)
}

func updateCameraConfig(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")
	defer request.Body.Close()

	var data RequestData
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	_, present := receivers[cameraID]
	if !present {
		http.Error(writer, fmt.Sprintf("Camera %s not found", cameraID), http.StatusNotFound)
		return
	}

	// TODO: if there are open SSE connections, just change the channel reference to the new object
	receivers[cameraID] = data.Receivers
	writer.WriteHeader(http.StatusOK)
}

// for future dev: consider making this asynchronous, as it will be called from the analysis program
// and it shouldn't be delayed too much
func sendNotification(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")
	log.Printf("[camera: %s] Received alert signal\n", cameraID)

	config, present := receivers[cameraID]
	if !present {
		http.Error(writer, fmt.Sprintf("Camera %s not found", cameraID), http.StatusNotFound)
		return
	}

	for _, receiver := range config {
		if len(receiver.SmtpServer) != 0 {
			log.Printf("[camera: %s] Sending email to %s\n", cameraID, receiver.SmtpRecipient)
			if err := sendEmail(cameraID, receiver); err != nil {
				log.Printf("[camera: %s] Could not send email: %v\n", cameraID, err)
			}
		}

		if receiver.UIPopup {
			if receiver.connOpen {
				log.Printf("[camera: %s] Sending UI alert\n", cameraID)
				receiver.eventsChannel <- fmt.Sprintf("Camera %s has detected suspicious behaviour", cameraID)
			} else {
				log.Printf("[camera: %s] SSE channel not open\n", cameraID)
			}
		}

		if len(receiver.WebhookURL) != 0 {
			log.Printf("[camera: %s] Sending request to webhook %s\n", cameraID, receiver.WebhookURL)
			if err := sendRequest(cameraID, receiver); err != nil {
				log.Printf("[camera: %s] Could not send webhook request: %v\n", cameraID, err)
			}
		}
	}
	writer.WriteHeader(http.StatusOK)
}

func notificationsPushChannel(w http.ResponseWriter, r *http.Request) {
	cameraID := chi.URLParam(r, "cameraID")

	config, present := receivers[cameraID]
	if !present {
		http.Error(w, fmt.Sprintf("Camera %s not found", cameraID), http.StatusNotFound)
		return
	}

	var eventsChannel chan string
	var receiverIdx int
	for i := range config {
		if config[i].UIPopup {
			if config[i].connOpen {
				http.Error(w, "SSE connection already open", http.StatusConflict)
				return
			}
			config[i].eventsChannel = make(chan string)
			config[i].connOpen = true
			eventsChannel = config[i].eventsChannel
			receiverIdx = i
			break
		}
	}

	defer func() {
		config := receivers[cameraID]
		config[receiverIdx].connOpen = false
		close(config[receiverIdx].eventsChannel)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	for {
		select {
		case message := <-eventsChannel:
			fmt.Fprint(w, message)
			flusher.Flush() // Push event to client
		case <-r.Context().Done():
			return
		}
	}
}

func sendRequest(cameraID string, config ReceiverConfig) error {
	var timeoutSecs int64 = 10
	if config.WebhookTimeout != 0 {
		timeoutSecs = config.WebhookTimeout
	}
	ctx, done := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer done()

	payload := fmt.Sprintf("Camera %s has detected suspicious behaviour", cameraID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.WebhookURL, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("could not create request: %v", err)
	}

	if len(config.WebhookCredentials) != 0 {
		authType := "basic"
		if len(config.WebhookAuthType) != 0 {
			authType = config.WebhookAuthType
		}
		creds, err := decryptCredentials(config.WebhookCredentials)
		if err != nil {
			return err
		}
		req.Header.Set("Authentication", fmt.Sprintf("%s %s", authType, creds))
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not send request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		bodyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("could not read webhook response body: %v", err)
		}
		return fmt.Errorf("webhook response error: %s", string(bodyBytes))
	}
	return nil
}

func sendEmail(cameraID string, config ReceiverConfig) error {
	// https://myaccount.google.com/apppasswords
	// Navigate to App Password Generator, designate an app name such as "security project," and obtain a 16-digit password.
	// Copy this password and paste it into the designated password field as instructed.

	creds, err := decryptCredentials(config.SmtpCredentials)
	if err != nil {
		return err
	}

	// maybe strip port from the server url?
	auth := smtp.PlainAuth("", config.SmtpSender, creds, config.SmtpServer)

	to := []string{config.SmtpRecipient}

	msg := []byte("Subject: Security Alert\r\n" +
		"\r\n" +
		"Camera " + cameraID + " has detected suspicious behaviour\r\n")

	//"smtp.gmail.com:587"
	return smtp.SendMail(config.SmtpServer, auth, config.SmtpSender, to, msg)
}

func decryptCredentials(credentials string) (string, error) {
	encryptedData, err := base64.StdEncoding.DecodeString(credentials)
	if err != nil {
		return "", fmt.Errorf("failed to decode credentials: %v", err)
	}

	decryptedBytes, err := rsa.DecryptPKCS1v15(nil, privateKey, encryptedData)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt credentials: %v", err)
	}
	return string(decryptedBytes), nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	log.Println("configuring server...")

	router := chi.NewRouter()

	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)
	router.Use(middleware.Heartbeat("/public/ping"))

	router.Post("/add", addCameraConfig)
	router.Patch("/update/{cameraID}", updateCameraConfig)
	router.Post("/alert/{cameraID}", sendNotification)
	router.Get("/notifications/{cameraID}", notificationsPushChannel)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           router,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	server.SetKeepAlivesEnabled(false)

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

	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()

	log.Println("shutting down server...")
	err := server.Shutdown(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}
}
