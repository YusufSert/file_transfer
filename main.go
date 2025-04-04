package main

import (
	"context"
	"fmt"
	"log"
	"pgm/services"
	"pgm/tools/logger"
	"pgm/tools/loki"
)

func main() {
	cfg, err := services.ReadPGMConfig("./config")
	if err != nil {
		log.Fatal(fmt.Errorf("pgm: error reading config file %w", err))
	}
	l, err := logger.NewLogger(cfg.LogFilePath) //todo: options pattern maybe for path and loglevel // what is inifunction in viper
	if err != nil {
		log.Fatal(err)
	}

	defer l.Close() // close the log file when main exits
	l.SetLevel(cfg.LogLevel)
	fmt.Println(l.Level)

	ctx := context.Background()

	t, err := loki.NewTail(cfg.LogFilePath, l.Logger)
	if err != nil {
		l.Logger.Error(err.Error())
	}

	t.Run(ctx)
	defer t.Close() // closes the log file when man exits

	pgm, err := services.NewPGMService(cfg, l.Logger)
	if err = pgm.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

//todo: check behavior of close(channel) with select statement.
