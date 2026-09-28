package downloader

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// 全局下载限速（字节/秒），0 = 不限速，所有任务共享令牌桶。
var speedLimit atomic.Int64

// SetSpeedLimit 设置全局限速。
func SetSpeedLimit(bytesPerSec int64) {
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	speedLimit.Store(bytesPerSec)
}

var (
	throttleMu    sync.Mutex
	throttleStart = time.Now()
	throttleBytes int64
)

// pacedReader 读多少 sleep 多少的简易令牌桶；不限速时零开销透传。
type pacedReader struct{ r io.Reader }

func (p pacedReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		pace(n)
	}
	return n, err
}

func pace(n int) {
	limit := speedLimit.Load()
	if limit <= 0 {
		return
	}
	for {
		throttleMu.Lock()
		now := time.Now()
		if now.Sub(throttleStart) >= time.Second {
			throttleStart = now
			throttleBytes = 0
		}
		if throttleBytes+int64(n) <= limit {
			throttleBytes += int64(n)
			throttleMu.Unlock()
			return
		}
		wait := time.Second - now.Sub(throttleStart)
		throttleMu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
	}
}
