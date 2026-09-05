package web

import (
	"encoding/json"
	"github.com/jeessy2/ddns-go/v6/config"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

// MemoryLogs 内存中的日志
type MemoryLogs struct {
	mu     sync.RWMutex
	MaxNum int      // 保存最大条数
	Logs   []string // 日志
}

func (mlogs *MemoryLogs) Write(p []byte) (n int, err error) {
	mlogs.mu.Lock()
	defer mlogs.mu.Unlock()
	mlogs.Logs = append(mlogs.Logs, string(p))
	// 处理日志数量
	if len(mlogs.Logs) > mlogs.MaxNum {
		mlogs.Logs = mlogs.Logs[len(mlogs.Logs)-mlogs.MaxNum:]
	}
	return len(p), nil
}

var mlogs = &MemoryLogs{MaxNum: 50}

// 初始化日志
func init() {
	log.SetOutput(io.MultiWriter(mlogs, os.Stdout))
	// log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

// Logs web
func Logs(writer http.ResponseWriter, request *http.Request) {
	// mlogs.Logs数组转为json
	mlogs.mu.RLock()
	logs, _ := json.Marshal(mlogs.Logs)
	mlogs.mu.RUnlock()
	writer.Write(logs)
}

// ClearLog
func ClearLog(writer http.ResponseWriter, request *http.Request) {
	mlogs.mu.Lock()
	mlogs.Logs = mlogs.Logs[:0]
	mlogs.mu.Unlock()
	c, _ := config.GetConfigCached()
	recordChange(request, c, "logs.clear", "", c.Cluster.LocalID, "application logs", "cleared")
}
