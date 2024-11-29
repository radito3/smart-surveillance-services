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
	"github.com/go-chi/cors"
)

type ReceiverConfig struct {
	UIPopup       bool        `json:"uiPopup,omitempty"` // default
	eventsChannel chan string `json:"-"`

	WebhookURL         string `json:"webhookUrl,omitempty"`
	WebhookTimeout     int64  `json:"webhookRequestTimeout,omitempty"` // default: 10s
	WebhookAuthType    string `json:"webhookAuthType,omitempty"`       // default: basic
	WebhookCredentials string `json:"webhookCredentials,omitempty"`    // encrypted

	SmtpServer      string `json:"smtpServer,omitempty"`
	SmtpSender      string `json:"smtpSender,omitempty"`
	SmtpRecipient   string `json:"smtpRecipient,omitempty"`
	SmtpCredentials string `json:"smtpCredentials,omitempty"` // encrypted
}

var receivers []ReceiverConfig
var privateKey *rsa.PrivateKey

func init() {
	privateKeyData, err := os.ReadFile("/etc/private-key/private_key.pem")
	if err != nil {
		log.Fatalf("failed to load private key: %v", err)
	}

	block, _ := pem.Decode(privateKeyData)
	privateKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		log.Fatalf("failed to parse private key: %v", err)
	}
}

func createConfig(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	if len(receivers) != 0 {
		http.Error(writer, "Configuration already present", http.StatusConflict)
		return
	}

	var data struct {
		Receivers []ReceiverConfig `json:"config"`
	}
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(data.Receivers) == 0 {
		http.Error(writer, "No receivers", http.StatusBadRequest)
		return
	}

	for i := range data.Receivers {
		if data.Receivers[i].UIPopup {
			data.Receivers[i].eventsChannel = make(chan string, 10)
			break
		}
	}

	// TODO: write to a volume, so that if the notification service has more than 1 instance,
	//  every one will have an up-to-date view of the data
	receivers = data.Receivers
	writer.WriteHeader(http.StatusCreated)
}

func updateConfig(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()

	var data struct {
		Receivers []ReceiverConfig `json:"config"`
	}
	err := json.NewDecoder(request.Body).Decode(&data)
	if err != nil {
		http.Error(writer, "Invalid JSON", http.StatusBadRequest)
		return
	}

	var hasUiPopup bool
	for i := range data.Receivers {
		if data.Receivers[i].UIPopup {
			hasUiPopup = true
			var hasCurrentUiPopup bool

			for j := range receivers {
				if receivers[j].eventsChannel != nil {
					hasCurrentUiPopup = true
					data.Receivers[i].eventsChannel = receivers[j].eventsChannel
					break
				}
			}
			if hasCurrentUiPopup {
				break
			} else {
				data.Receivers[i].eventsChannel = make(chan string, 10)
			}
		}
	}

	if !hasUiPopup {
		for i := range receivers {
			if receivers[i].eventsChannel != nil {
				close(receivers[i].eventsChannel)
			}
		}
	}

	// TODO: write to a volume, so that if the notification service has more than 1 instance,
	//  every one will have an up-to-date view of the data
	receivers = data.Receivers
	writer.WriteHeader(http.StatusOK)
}

func deleteConfig(writer http.ResponseWriter, request *http.Request) {
	for i := range receivers {
		if receivers[i].eventsChannel != nil {
			close(receivers[i].eventsChannel)
		}
	}

	// TODO: write to a volume, so that if the notification service has more than 1 instance,
	//  every one will have an up-to-date view of the data
	receivers = nil
	writer.WriteHeader(http.StatusNoContent)
}

// for future dev: consider making this asynchronous, as it will be called from the analysis program
// and it shouldn't be delayed too much
func sendNotification(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")
	log.Printf("[camera: %s] Received alert signal\n", cameraID)

	for _, receiver := range receivers {
		if len(receiver.SmtpServer) != 0 {
			log.Printf("[camera: %s] Sending email to %s\n", cameraID, receiver.SmtpRecipient)
			if err := sendEmail(cameraID, receiver); err != nil {
				log.Printf("[camera: %s] Could not send email: %v\n", cameraID, err)
			}
		}

		if receiver.UIPopup {
			log.Printf("[camera: %s] Sending UI alert\n", cameraID)
			select {
			case receiver.eventsChannel <- fmt.Sprintf("Camera %s has detected suspicious behaviour", cameraID):
			default:
				log.Printf("[camera: %s] Unable to send UI alert: SSE channel closed\n", cameraID)
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

// for future dev:
// if multiple instances of the Web UI are supported, we need a message passing mechanism
// we need to keep track of the currently open TCP connections (the Web UIs)
// and distribute the notification messages to each one
// the issue comes from the fact that the consumers may change dynamically (adding/removing)
func notificationsPushChannel(w http.ResponseWriter, r *http.Request) {
	var eventsChannel chan string
	for i := range receivers {
		if receivers[i].UIPopup {
			eventsChannel = receivers[i].eventsChannel
			break
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, ": keep-alive\n\n")
	flusher.Flush()

	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case message, chanOpen := <-eventsChannel:
			if !chanOpen {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", message)
			flusher.Flush() // Push event to client
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-interruptChan:
			return
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
		req.Header.Set("Authentication", fmt.Sprintf("%s %s", authType, string(creds)))
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

// can test with https://aiosmtpd.aio-libs.org/en/stable/index.html
func sendEmail(cameraID string, config ReceiverConfig) error {
	// https://myaccount.google.com/apppasswords
	// Navigate to App Password Generator, designate an app name such as "security project," and obtain a 16-digit password.
	// Copy this password and paste it into the designated password field as instructed.

	creds, err := decryptCredentials(config.SmtpCredentials)
	if err != nil {
		return err
	}

	host := config.SmtpServer
	portSeparatorIdx := strings.IndexByte(host, ':')
	if portSeparatorIdx != -1 {
		host = host[:portSeparatorIdx]
	}
	auth := smtp.PlainAuth("", config.SmtpSender, string(creds), host)

	to := []string{config.SmtpRecipient}

	msg := []byte("Subject: Security Alert\r\n" +
		"\r\n" +
		"Camera " + cameraID + " has detected suspicious behaviour\r\n")

	// example server: smtp.gmail.com:587
	return smtp.SendMail(config.SmtpServer, auth, config.SmtpSender, to, msg)
}

func decryptCredentials(credentials string) ([]byte, error) {
	encryptedData, err := base64.StdEncoding.DecodeString(credentials)
	if err != nil {
		return nil, fmt.Errorf("failed to decode credentials: %v", err)
	}

	decryptedBytes, err := rsa.DecryptPKCS1v15(nil, privateKey, encryptedData)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt credentials: %v", err)
	}
	return decryptedBytes, nil
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
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	router.Post("/config", createConfig)
	router.Patch("/config", updateConfig)
	router.Delete("/config", deleteConfig)
	router.Post("/alert/{cameraID}", sendNotification)
	router.Get("/notifications-stream", notificationsPushChannel)

	server := &http.Server{
		Addr:              ":8081",
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

	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()

	log.Println("shutting down server...")
	err := server.Shutdown(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}
}
