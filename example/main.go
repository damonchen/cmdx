package main

import (
	"fmt"
	"github.com/damonchen/cmdx"
	"time"
)

func main() {
	c := "./test.sh"
	cmd, readerCloser := cmdx.NewCombinedPipeCmd(c, cmdx.WithArgs("-alh"), cmdx.WithTimeout(10))
	defer readerCloser.Close()

	t := time.Now()
	go func() {
		var buff [20]byte

		tick := cmd.Tick()
		for {
			select {
			case <-time.After(time.Second * 5):
				fmt.Println("cancel")
				cmd.Cancel()
				return
			case <-cmd.Done():
				fmt.Println("timeout", time.Since(t))
				return
			case <-tick:
				_, err := readerCloser.Read(buff[:])
				if err != nil {
					fmt.Println("read error", err, time.Since(t))
					return
				}
				fmt.Println(string(buff[:]))
			}
		}
	}()

	err := cmd.Run()
	if err != nil {
		fmt.Println(err, time.Since(t))
	}
	fmt.Println("since", time.Since(t), cmd.Status())
	time.Sleep(time.Second)
	return
}
