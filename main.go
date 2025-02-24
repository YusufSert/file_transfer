package main

import (
	"context"
	"fmt"
	"log"
	"pgm/filetransfer"
	"sync"
)

func main() {

	/*
	   		ftp, err := filetransfer.NewFTPService("localhost:21")
	   		if err != nil {
	   			log.Fatal(err)
	   		}

	   		err = ftp.Login("yusufcan", "Banana@@")
	   		if err != nil {
	   			log.Fatal(err)
	   		}
	   		start := make(chan any)

	   		go func() {
	   			<-start
	   			for {
	   				fi, err := ftp.ListFiles("/home/yusufcan/Desktop/ftp_write")
	   				if err != nil {
	   					fmt.Println(err)
	   				}
	   				fmt.Println(fi[0].ModTime(), fi[0].Name())
	   			}
	   		}()
	   		go func() {
	   			<-start
	   			for {
	   				fi2, err := ftp.ListFiles("/home/yusufcan/Desktop/ftp_write")
	   				if err != nil {
	   					fmt.Println(err)
	   				}
	   				fmt.Println(fi2[0].ModTime(), fi2[0].Name())
	   			}
	   		}()

	   		go func() {
	   			time.Sleep(1 * time.Second)
	   			close(start)
	   		}()

	   		time.Sleep(5 * time.Second)

	   		f, err := os.Open("/home/yusufcan/Desktop/local_read/t2.txt")
	   		err = ftp.Store(filetransfer.Info("/home/yusufcan/Desktop/ftp_write/t2.txt"), f)
	   		if err != nil {
	   			log.Fatal(err)
	   		}

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
	       		HeartBeatInterval:   time.Second,
	       	}

	       	pgm, err := services.NewPGMService(cfg)

	       	err = pgm.Run(context.Background())
	       	log.Fatal(err)
	*/
	s, err := filetransfer.Open("localhost:21", "yusufcan", "Banana@@")
	if err != nil {
		fmt.Println(err)
	}
	var wg sync.WaitGroup

	var count int
	for count < 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err = s.ListFilesContext(context.Background(), "/home/yusufcan/Desktop/ftp_read")
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%+v\n", s.Stats())
		}()
		count++
		fmt.Println(count)
	}

	wg.Wait()

}
