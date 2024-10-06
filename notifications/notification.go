package notifications

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/smtp"

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
		log.Fatal(err)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.LUTC)
	log.SetPrefix("[debug] ")

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

	// wait on signal

	s.GracefulStop()
}
