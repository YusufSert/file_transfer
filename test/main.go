package main

import (
	"errors"
	"fmt"
	"log"
	"os"
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
	fmt.Println(b)

	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ltime | log.LUTC)

	doWork := func(done <-chan interface{}, _ time.Duration) <-chan interface{} {
		log.Println("ward: Hello, I'm irresponsible!")
		go func() {
			<-done
			log.Println("ward: I am halting.")
		}()
		return nil
	}
	doWorkWithSteward := newSteward(4*time.Second, doWork)
	done := make(chan interface{})
	time.AfterFunc(9*time.Second, func() {
		log.Println("main: halting steward and ward.")
		close(done)
	})
	for range doWorkWithSteward(done, 4*time.Second) {
	}
	log.Println("Done")

}

type startGoroutineFn func(done <-chan interface{}, pulseInterval time.Duration) <-chan interface{}

func newSteward(timeout time.Duration, fn startGoroutineFn) startGoroutineFn {
	return func(done <-chan interface{}, pulseInterval time.Duration) <-chan interface{} {
		heartbeat := make(chan interface{})
		go func() {
			defer close(heartbeat)

			var wardDone chan interface{}
			var wardHeartBeat <-chan interface{}
			startWard := func() {
				wardDone = make(chan interface{})
				wardHeartBeat = fn(or(wardDone, done), timeout/2)
			}
			startWard()
			pulse := time.Tick(pulseInterval)

		monitorLoop:
			for {
				timeoutSignal := time.After(timeout)

				for {
					select {
					case <-pulse:
						select {
						case heartbeat <- struct{}{}:
						default:
						}
					case <-wardHeartBeat:
						continue monitorLoop
					case <-timeoutSignal:
						log.Println("steward: ward unhealthy; restarting")
						close(wardDone)
						startWard()
						continue monitorLoop
					case <-done:
						return
					}
				}
			}
		}()
		return heartbeat
	}
}

func or(channels ...<-chan interface{}) <-chan interface{} {
	switch len(channels) {
	case 0:
		return nil
	case 1:
		return channels[0]
	}

	orDone := make(chan interface{})
	go func() {
		defer close(orDone)

		switch len(channels) {
		case 2:
			select {
			case <-channels[0]:
			case <-channels[1]:
			}
		default:
			select {
			case <-channels[0]:
			case <-channels[1]:
			case <-channels[2]:
			case <-or(append(channels[3:], orDone)...):
			}
		}
	}()
	return orDone
}

func orDoneFn(done, c <-chan interface{}) <-chan interface{} {
	valStream := make(chan interface{})
	go func() {
		defer close(valStream)
		for {
			select {
			case <-done:
				return
			case v, ok := <-c:
				if ok == false {
					return
				}
				select {
				case valStream <- v:
				case <-done:
				}
			}
		}
	}()
	return valStream
}

func retry(f func() error) error {
	for i := 0; i < 2; i++ {
		err := f()
		if err == nil || !errors.Is(err, errString) { // error nil ise veya retryable error değilse return
			return err
		}
	}
	//Last try
	return f()
}

func conn() (string, error) {
	return "go", nil
}

var errString error = errors.New("string error")

func doWorkj(
	done <-chan interface{},
	intList ...int,
) (startGoroutineFn, <-chan interface{}) {
	intChanStream := make(chan (<-chan interface{}))
	intStream := bridge(done, intChanStream)
	doWork := func(
		done <-chan interface{},
		pulseInterval time.Duration,
	) <-chan interface{} {
		intStream := make(chan interface{})
		heartbeat := make(chan interface{})
		go func() {
			defer close(intStream)
			select {
			case intChanStream <- intStream:
			case <-done:
				return
			}
			pulse := time.Tick(pulseInterval)
			for {
			valueLoop:
				for _, intVal := range intList {
					if intVal < 0 {
						log.Printf("negative value: %v\n", intVal)
						return
					}
					for {
						select {
						case <-pulse:
							select {
							case heartbeat <- struct{}{}:
							default:
							}
						case intStream <- intVal:
							continue valueLoop
						case <-done:
							return
						}
					}
				}
			}
		}()
		return heartbeat
	}
	return doWork, intStream
}
bridge := func(
done <-chan interface{},
chanStream <-chan <-chan interface{},
) <-chan interface{} {
valStream := make(chan interface{})
go func() {
defer close(valStream)
for {
var stream <-chan interface{}
select {
case maybeStream, ok := <-chanStream:
if ok == false {
return
}
stream = maybeStream
case <-done:
return
}
for val := range orDone(done, stream) {
select {
case valStream <- val:
case <-done:
}
}
}
122
|
Chapter 4: Concurrency Patterns in Go}()
return valStream
}
