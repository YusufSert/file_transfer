package main

import (
	"context"
	"log"
	"pgm/services"
	"pgm/tools/logger"
	"time"
)

func main() {

	cfg := services.PGMConfig{
		Addr:                "localhost:21",
		User:                "yusufcan",
		Password:            "Banana@@",
		NetworkToUploadPath: "/home/yusufcan/Desktop/network_toupload",
		NetworkOutgoingPath: "/home/yusufcan/Desktop/network_outgoing",
		NetworkIncomingPath: "/home/yusufcan/Desktop/network_incoming",
		FTPWritePath:        "/home/yusufcan/Desktop/ftp_write",
		FTPReadPath:         "/home/yusufcan/Desktop/ftp_read",
		PoolInterval:        time.Second * 5,
		HeartBeatInterval:   time.Second * 1,
	}

	agent, err := logger.NewLogAgent()
	if err != nil {
		log.Fatal(err)
	}
	// todo: loglara caller field ekle we cağıran file adı olsun main.go, fpt_service.go
	pgm, err := services.NewPGMService(cfg, agent)
	ctx := context.Background()
	go agent.Run(ctx)

	//todo: check behavior of close(channel) with select statement.

	err = pgm.Run(ctx)
	log.Fatal(err)
}
