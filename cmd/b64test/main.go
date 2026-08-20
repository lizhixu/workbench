package main

import (
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	data, _ := os.ReadFile("bin/linux_amd64/watchman-agent")
	fmt.Println("file size:", len(data))
	chunkSize := 48 * 1024
	chunks := (len(data) + chunkSize - 1) / chunkSize
	fmt.Println("chunks:", chunks)
	totalB64Len := 0
	totalDecoded := 0
	for i := 0; i < chunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		b64 := base64.StdEncoding.EncodeToString(data[start:end])
		decoded, _ := base64.StdEncoding.DecodeString(b64)
		totalB64Len += len(b64)
		totalDecoded += len(decoded)
		if i == 0 {
			fmt.Printf("chunk 0: raw=%d b64=%d decoded=%d\n", end-start, len(b64), len(decoded))
		}
	}
	fmt.Println("total b64 len:", totalB64Len)
	fmt.Println("total decoded:", totalDecoded)
	fmt.Println("match:", totalDecoded == len(data))
}
