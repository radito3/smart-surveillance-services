package notifications

import (
	"log"
	"net/smtp"
)

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
