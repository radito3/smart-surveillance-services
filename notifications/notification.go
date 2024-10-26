package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
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
	UIPopup       bool   `json:"ui_popup,omitempty"` // default
	SmtpServer    string `json:"smtp_server,omitempty"`
	SmtpRecipient string `json:"smtp_recipient,omitempty"`
	// ...
}

var receivers map[string][]ReceiverConfig = make(map[string][]ReceiverConfig)

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
	receivers[cameraID] = data.Receivers
	writer.WriteHeader(http.StatusOK)
}

func sendNotification(writer http.ResponseWriter, request *http.Request) {
	cameraID := chi.URLParam(request, "cameraID")
	log.Printf("received alert signal from %s", cameraID)

	config, present := receivers[cameraID]
	if !present {
		http.Error(writer, fmt.Sprintf("Camera %s not found", cameraID), http.StatusNotFound)
		return
	}

	for _, receiver := range config {
		// TODO: impl
		log.Printf("sending alert to %v", receiver)

		if len(receiver.SmtpServer) != 0 {
			if err := sendEmail(cameraID, receiver); err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	writer.WriteHeader(http.StatusOK)
}

func sendEmail(cameraID string, config ReceiverConfig) error {
	//  https://myaccount.google.com/apppasswords
	//  Navigate to App Password Generator, designate an app name such as "security project," and obtain a 16-digit password.
	//  Copy this password and paste it into the designated password field as instructed.
	auth := smtp.PlainAuth("", "john.doe@gmail.com", "extremely_secret_pass", "smtp.gmail.com")

	to := []string{config.SmtpRecipient}

	msg := []byte("To: kate.doe@example.com\r\n" +
		"Subject: Security Alert\r\n" +
		"\r\n" +
		"Camera " + cameraID + " has detected suspicious behaviour\r\n")

	//"smtp.gmail.com:587"
	return smtp.SendMail(config.SmtpServer, auth, "john.doe@gmail.com", to, msg)
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

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()

	log.Println("shutting down server...")
	err := server.Shutdown(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
	}
}
