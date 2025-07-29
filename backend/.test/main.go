package main

import (
	"fmt"
	"strings"
	"time"
)

// 定义状态常量
const (
	StateRunning = "running"
	StatePaused  = "paused"
	StateStopped = "stopped"
)

// 通道默认会连续读取直到结束, 需借助另一个通道来实现暂停、恢复和停止
func main() {
	dataCh := make(chan string, 10000)
	ctrlCh := make(chan string)
	doneCh := make(chan struct{})

	// 接收协程
	go func() {
		defer close(doneCh)
		state := StateRunning // 初始状态为运行

		for {
			switch state {
			case StatePaused:
				state = <-ctrlCh // 等待阻塞解除信号
				continue
			case StateStopped:
				return
			case StateRunning:
				select {
				case newState := <-ctrlCh:
					state = newState
				case data, ok := <-dataCh:
					if !ok {
						fmt.Println("========= 数据读取完毕，自然结束")
						return
					}
					fmt.Println("处理数据:", data)
					time.Sleep(time.Second)
				}
			}
		}
	}()

	// 控制协程
	go func() {
		time.Sleep(3 * time.Second)

		ctrlCh <- StatePaused
		time.Sleep(3 * time.Second)

		ctrlCh <- StateRunning
		time.Sleep(3 * time.Second)

		ctrlCh <- StatePaused
		time.Sleep(3 * time.Second)

		ctrlCh <- StateRunning
		time.Sleep(2 * time.Second)

		ctrlCh <- StateStopped
	}()

	// 发送协程
	s := "i love this beautiful world and i want to share it with you all"
	ss := strings.Split(s, " ")
	for _, v := range ss {
		dataCh <- v
	}
	close(dataCh)

	<-doneCh
}
