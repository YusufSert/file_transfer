package main

import (
	"context"
	"log"
	"os"
	"pgm/services"
	"pgm/tools/logger"
	"pgm/tools/loki"
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
	r, err := os.OpenFile("./log2.txt", os.O_RDONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	w, err := os.OpenFile("./log2.txt", os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	l, err := logger.NewLogger(w)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	t, err := loki.NewTail(r, l.Logger)
	go t.Run(ctx)

	pgm, err := services.NewPGMService(cfg, l.Logger)
	if err = pgm.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

//todo: check behavior of close(channel) with select statement.
