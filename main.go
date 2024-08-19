package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	notif "github.com/sss/notifications"

	grpc "google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Server struct {
	notif.UnimplementedNotificationDelegateServiceServer
}

func (s *Server) SendNotification(ctx context.Context, in *notif.HelloRequest) (*emptypb.Empty, error) {
	log.Printf("Received: %v", in.GetName())
	// send to all receivers
	return &emptypb.Empty{}, nil
}

func addCamera(w http.ResponseWriter, r *http.Request) {
	_ = r.URL.Query()
	_ = r.URL.Fragment

	body := strings.NewReader("{\"a\":\"test\"}")
	res, err := http.Post("http://localhost:9997/v3/config/paths/add/{name}", "application/json", body)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}

	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		w.WriteHeader(res.StatusCode)
		return
	}

	w.WriteHeader(200)
}

func getEndpoints(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
}

func getEndpointEncodings(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
}

func getPort() string {
	portFromEnv, isSet := os.LookupEnv("PORT")
	if !isSet {
		return "8080"
	}
	return portFromEnv
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

	// log.Println("configuring server...")
	// mux := http.NewServeMux()
	// mux.HandleFunc("/addCamera", addCamera)
	// mux.HandleFunc("/list", getEndpoints)                // return all endpoints
	// mux.HandleFunc("/list/{path}", getEndpointEncodings) // for transcoding manifest
	// // TODO: remove camera endpoint

	// server := &http.Server{
	// 	Addr:              ":" + getPort(),
	// 	Handler:           mux,
	// 	ReadTimeout:       5 * time.Second,
	// 	ReadHeaderTimeout: 2 * time.Second,
	// 	WriteTimeout:      10 * time.Second,
	// }
	// server.SetKeepAlivesEnabled(false)

	// go func() {
	// 	log.Println("starting server...")
	// 	// TODO: make an self-signed SSL certificate
	// 	err := server.ListenAndServe()
	// 	if err != nil && err != http.ErrServerClosed {
	// 		fmt.Fprintln(os.Stderr, err.Error())
	// 	}
	// 	log.Println("server stopped")
	// }()

	// TODO: use a secure connection
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 50051))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	notif.RegisterNotificationDelegateServiceServer(s, &Server{})
	log.Printf("server listening at %v", lis.Addr())

	go func() {
		if err := s.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	<-signalChan

	// ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	// defer done()

	log.Println("shutting down server...")
	s.GracefulStop()
	// err := server.Shutdown(ctx)
	// if err != nil {
	// 	fmt.Fprintln(os.Stderr, err.Error())
	// }
}
