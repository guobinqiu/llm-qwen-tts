package main

import (
	"fmt"
	"strings"
	"time"
)

// 通道默认会连续读取直到结束, 需借助另一个通道来实现暂停和断点续读
func main() {
	dataCh := make(chan string, 10000)
	ctrlCh := make(chan bool) // 通道控制变量(paused)
	doneCh := make(chan struct{})

	// 接收协程
	go func() {
		defer close(doneCh)
		paused := false // 变量控制流程

		for {
			if paused {
				cmd := <-ctrlCh // 等待解除阻塞, 收到true下一轮重新阻塞, 收到false下一轮继续数据读取
				paused = cmd
				continue
			}

			select {
			case <-ctrlCh:
				paused = true
			case data, ok := <-dataCh:
				if !ok {
					return
				}
				fmt.Println(data)
				time.Sleep(time.Second)
			}
		}
	}()

	// 控制协程
	go func() {
		fmt.Println("========= 运行3秒")
		time.Sleep(3 * time.Second)

		fmt.Println("========= 暂停3秒")
		ctrlCh <- true // pause
		time.Sleep(3 * time.Second)

		fmt.Println("========= 运行3秒")
		ctrlCh <- false // resume
		time.Sleep(3 * time.Second)

		fmt.Println("========= 暂停3秒")
		ctrlCh <- true // pause
		time.Sleep(3 * time.Second)

		fmt.Println("========= 运行到结束")
		ctrlCh <- false // resume
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
