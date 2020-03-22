package main

import (
	"fmt"
	"os"

	"github.com/stdchat/tgbot"
	"stdchat.org/provider"
	"stdchat.org/service"
)

func main() {
	err := provider.Run(tgbot.Protocol,
		func(t service.Transporter) service.Servicer {
			return tgbot.NewService(t)
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
