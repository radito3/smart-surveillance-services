package notifications

import (
	"context"
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

type ReceiverConfig struct {
	UIPopup       bool   `json:"ui_popup,omitempty"`
	SmtpServer    string `json:"smtp_server,omitempty"`
	SmtpRecipient string
	// ...
}

var receivers map[string][]ReceiverConfig = make(map[string][]ReceiverConfig)

func createReceiver(w http.ResponseWriter, r *http.Request) {
	// TODO: read from request body the type of receivers
	// the camera ID will be in the path
}

func sendNotification(w http.ResponseWriter, r *http.Request) {
	log.Printf("received alert signal from {camera ID}")
	// TODO: send to all receivers
	_ = receivers["a"]
}

// TODO: do not bother with a full mailing implementation, just write about it in the thesis paper
func sendEmail() {
	//  https://myaccount.google.com/apppasswords
	//  Navigate to App Password Generator, designate an app name such as "security project," and obtain a 16-digit password.
	//  Copy this password and paste it into the designated password field as instructed.

	auth := smtp.PlainAuth("", "john.doe@gmail.com", "extremely_secret_pass", "smtp.gmail.com")

	to := []string{"kate.doe@example.com"}

	msg := []byte("To: kate.doe@example.com\r\n" +
		"Subject: Security Alert\r\n" +
		"\r\n" +
		"Camera {N} has detected suspicios behaviour\r\n")

	err := smtp.SendMail("smtp.gmail.com:587", auth, "john.doe@gmail.com", to, msg)

	if err != nil {
		log.Println(err)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	log.Println("configuring server...")

	router := chi.NewRouter()

	router.Use(middleware.Recoverer)
	router.Use(middleware.CleanPath)
	router.Use(middleware.Heartbeat("/public/ping"))

	router.Post("/add/{cameraID}", sendNotification)
	router.Post("/alert", sendNotification)

	server := &http.Server{
		Addr:              ":80",
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
