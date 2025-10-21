package main

import (
	"log"
	"os"
	"os/signal"
	"pilot/internal/gateway"
	"syscall"

	"github.com/spf13/viper"
)

func main() {
	conf := gateway.DefaultConfig()
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./config")
	if err := viper.ReadInConfig(); err != nil {
		log.Printf("Config file not found, using default config: %v", err)
	} else {
		if err := viper.Unmarshal(conf); err != nil {
			log.Printf("Failed to unmarshal config: %v, using default config", err)
		}
	}

	gw, err := gateway.NewHTTPGateway(conf)
	if err != nil {
		log.Fatalf("Failed to create gateway: %v", err)
	}

	if err := gw.Start(); err != nil {
		log.Fatalf("Failed to start gateway: %v", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	if err := gw.Stop(); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}
}
