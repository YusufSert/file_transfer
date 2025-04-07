package main

import (
	"context"
	"fmt"
	"log"
	"pgm/app"
	"pgm/repo"
	"pgm/tools/logger"
	"pgm/tools/loki"
)

func main() {
	cfg, err := app.ReadPGMConfig("./config")
	if err != nil {
		log.Fatal(fmt.Errorf("pgm: error reading config file %w", err))
	}
	l, err := logger.NewLogger(cfg.LogFilePath) //todo: options pattern maybe for path and loglevel // what is inifunction in viper
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close() // close the log file when main exits
	l.SetLevel(cfg.LogLevel)

	ctx := context.Background()

	t, err := loki.NewTail(cfg.LogFilePath, l.Logger)
	if err != nil {
		l.Logger.Error(err.Error())
	}
	t.Run(ctx)
	defer t.Close() // closes the log file when man exits

	r, err := repo.NewPGMRepo(cfg.DBConnStr)
	if err != nil {
		l.Logger.Error(err.Error())
	}

	pgm, err := app.NewPGMService(cfg, r, l.Logger)
	fmt.Println(pgm)
	//todo give pgm to win_service
}

//todo: check behavior of close(channel) with select statement.
