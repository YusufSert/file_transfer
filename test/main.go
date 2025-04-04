package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"time"
)

func main() {
	// Fast version (changes order)
	a := []string{"A", "B", "C", "D", "E"}
	i := 2
	noew := time.Now()
	// Remove the element at index i from a.
	a[i] = a[len(a)-1] // Copy last element to index i
	a[len(a)-1] = ""   // Erase last element (write zero value)
	a = a[:len(a)-1]   // Truncate slice.
	fmt.Println(time.Since(noew))
	fmt.Println(a)

	// Slow version
	b := []string{"A", "B", "C", "D", "E"}

	noew = time.Now()
	// Remove the element at index i from b
	copy(b[i:], b[i+1:]) // Shift b[i+1:] left one index.
	b[len(b)-1] = ""     // Erase last element (write zero value).
	b = b[:len(b)-1]     // Truncate slice

	/*
		err := os.Rename("./log3.txt", "./test/log2.txt")

		if err != nil {
			var eN syscall.Errno
			if errors.As(err, &eN) {
				fmt.Println(uint(eN))
			}
			fmt.Println(eN)
		}
	*/
	/*
	   	   fileW, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	   	   if err != nil {
	   	       log.Fatal(err.Error())
	   	   }
	   	   defer fileW.Close()
	   	   l := slog.New(slog.NewJSONHandler(fileW, nil)).WithGroup("data")
	   	   for i := 0; i < 10; i++ {
	   	       l.Error("error log", "err", errors.New("test-error"))
	   	       l.Info("info log", "id", 1000+i)
	   	   }
	       	file, err := os.Open(filePath)
	       	if err != nil {
	       		log.Fatal(err)
	       	}

	       	reader := bufio.NewReader(file)
	       	var pos uint64
	       	for {
	       		record, err := reader.ReadBytes('\n')
	       		if err != nil {
	       			if err == io.EOF {
	       				time.Sleep(1 * time.Second)
	       				fmt.Println(err)
	       			}
	       		}
	       		pos += uint64(len(record))
	       		fmt.Println(pos)

	       		if err == nil {
	       			var r Record
	       			err = json.Unmarshal(record, &r)
	       			if err != nil {
	       				log.Fatal(err)
	       			}
	       			fmt.Printf("%+v\n", r)
	       		}
	       	}
	*/
	h := sha1.New()
	_, err := h.Write([]byte("k"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(fmt.Sprintf("%x", h.Sum(nil)[:4]), nil) // learn bytes are thet hexadecimal ??? what is one byte in string or number
	fmt.Println(time.Now().Format("2006"))
}

var base string = "./SANTRAL/ORTAK/SCS/OFIS_DESTEK/POLIS/PGM"

type bok struct {
	year int
}

// todo: maybe implement function that returns dir.
func (b *bok) test(dir string) (string, error) {
	currYear := time.Now().Year()
	var fullPath string
	if b.year == currYear {
		fullPath = path.Join(base, strconv.Itoa(b.year), dir)
		return fullPath, nil
	}

	b.year = currYear

	fullPath = path.Join(base, strconv.Itoa(b.year), dir)
	err := os.MkdirAll(fullPath, 0750)
	if err == nil || errors.Is(err, os.ErrExist) {
		return fullPath, nil
	}
	return "", err
}
