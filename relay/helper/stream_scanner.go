package helper

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

const (
	InitialScannerBufferSize     = 64 << 10 // 64KB (64*1024)
	DefaultMaxScannerBufferSize  = 64 << 20 // 64MB (64*1024*1024) default SSE buffer size
	DefaultPingInterval          = 10 * time.Second
	DefaultWriteTimeout          = 10 * time.Second
	DefaultWriteEnqueueTimeout   = 2 * time.Second
	DefaultWriteQueueSize        = 8
	DefaultCleanupWaitTimeout    = 2 * time.Second
	DefaultDisconnectWaitTimeout = 1 * time.Second
)

func getScannerBufferSize() int {
	if constant.StreamScannerMaxBufferMB > 0 {
		return constant.StreamScannerMaxBufferMB << 20
	}
	return DefaultMaxScannerBufferSize
}

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string) bool) {
	if resp == nil || dataHandler == nil {
		return
	}

	streamingTimeout := time.Duration(constant.StreamingTimeout) * time.Second
	writeTimeout := DefaultWriteTimeout
	writeEnqueueTimeout := DefaultWriteEnqueueTimeout
	writeQueueSize := DefaultWriteQueueSize
	cleanupWaitTimeout := DefaultCleanupWaitTimeout
	disconnectWaitTimeout := DefaultDisconnectWaitTimeout

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, InitialScannerBufferSize), getScannerBufferSize())
	scanner.Split(bufio.ScanLines)
	SetEventStreamHeaders(c)

	generalSettings := operation_setting.GetGeneralSetting()
	pingEnabled := generalSettings.PingIntervalEnabled && !info.DisablePing
	pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}

	if common.DebugEnabled {
		println("relay timeout seconds:", common.RelayTimeout)
		println("relay max idle conns:", common.RelayMaxIdleConns)
		println("relay max idle conns per host:", common.RelayMaxIdleConnsPerHost)
		println("streaming timeout seconds:", int64(streamingTimeout.Seconds()))
		println("ping interval seconds:", int64(pingInterval.Seconds()))
		println("write timeout seconds:", int64(writeTimeout.Seconds()))
		println("write enqueue timeout ms:", writeEnqueueTimeout.Milliseconds())
		println("write queue size:", writeQueueSize)
		println("cleanup wait timeout ms:", cleanupWaitTimeout.Milliseconds())
		println("disconnect wait timeout ms:", disconnectWaitTimeout.Milliseconds())
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	var requestDone <-chan struct{}
	if c.Request != nil {
		requestDone = c.Request.Context().Done()
	}

	const (
		cancelReasonNone int32 = iota
		cancelReasonWriteError
		cancelReasonHandlerStop
		cancelReasonWriteEnqueueTimeout
		cancelReasonWriteTaskTimeout
		cancelReasonClientDisconnected
	)
	var cancelReason atomic.Int32
	var clientDisconnected atomic.Bool
	setCancelReason := func(reason int32) {
		if reason != cancelReasonNone {
			cancelReason.CompareAndSwap(cancelReasonNone, reason)
		}
		cancel()
	}

	var closeRespBodyOnce sync.Once
	closeRespBody := func() {
		closeRespBodyOnce.Do(func() {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
		})
	}
	onClientDisconnected := func() {
		clientDisconnected.Store(true)
		setCancelReason(cancelReasonClientDisconnected)
		closeRespBody()
	}
	if requestDone != nil {
		go func() {
			select {
			case <-requestDone:
				onClientDisconnected()
			case <-ctx.Done():
			}
		}()
	}

	type streamWriteResult struct {
		shouldContinue bool
		err            error
	}
	type streamWriteTask struct {
		kind   string
		data   string
		result chan streamWriteResult
	}
	type streamScannerEvent struct {
		data     string
		done     bool
		activity bool
		err      error
	}

	writeTaskChan := make(chan streamWriteTask, writeQueueSize)
	writeWorkerDone := make(chan struct{})
	go func() {
		defer close(writeWorkerDone)
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(c, "write worker panic recovered")
				cancel()
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case task, ok := <-writeTaskChan:
				if !ok {
					return
				}

				result := streamWriteResult{shouldContinue: true}
				switch task.kind {
				case "ping":
					result.err = PingData(c)
				case "data":
					result.shouldContinue = dataHandler(task.data)
				}

				// 任一写入失败/handler 主动终止时，尽快取消连接，避免写入拥塞扩散。
				if result.err != nil {
					setCancelReason(cancelReasonWriteError)
				}
				if !result.shouldContinue {
					setCancelReason(cancelReasonHandlerStop)
				}

				select {
				case task.result <- result:
				default:
				}
			}
		}
	}()

	scannerEventChan := make(chan streamScannerEvent, 32)
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		defer close(scannerEventChan)
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(c, "scanner goroutine panic recovered")
				cancel()
			}
		}()

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			if common.DebugEnabled {
				println(line)
			}

			if strings.HasPrefix(line, "[DONE]") {
				select {
				case scannerEventChan <- streamScannerEvent{done: true}:
				case <-ctx.Done():
				}
				return
			}
			if !strings.HasPrefix(line, "data:") {
				select {
				case scannerEventChan <- streamScannerEvent{activity: true}:
				case <-ctx.Done():
				}
				continue
			}

			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimLeft(data, " ")
			data = strings.TrimSuffix(data, "\r")
			if strings.HasPrefix(data, "[DONE]") {
				select {
				case scannerEventChan <- streamScannerEvent{done: true}:
				case <-ctx.Done():
				}
				return
			}
			if len(data) == 0 {
				select {
				case scannerEventChan <- streamScannerEvent{activity: true}:
				case <-ctx.Done():
				}
				continue
			}

			select {
			case scannerEventChan <- streamScannerEvent{data: data}:
			case <-ctx.Done():
				return
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			select {
			case scannerEventChan <- streamScannerEvent{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	streamingTimer := time.NewTimer(streamingTimeout)
	defer streamingTimer.Stop()

	writeTimeoutTimer := time.NewTimer(writeTimeout)
	if !writeTimeoutTimer.Stop() {
		select {
		case <-writeTimeoutTimer.C:
		default:
		}
	}
	defer writeTimeoutTimer.Stop()

	var pingTicker *time.Ticker
	if pingEnabled {
		pingTicker = time.NewTicker(pingInterval)
		defer pingTicker.Stop()
	}
	var pingC <-chan time.Time
	if pingTicker != nil {
		pingC = pingTicker.C
	}

	resetTimer := func(timer *time.Timer, duration time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(duration)
	}

	dispatchWriteTask := func(kind, data, timeoutMessage string) (streamWriteResult, bool) {
		resultChan := make(chan streamWriteResult, 1)
		task := streamWriteTask{
			kind:   kind,
			data:   data,
			result: resultChan,
		}

		enqueueTimer := time.NewTimer(writeEnqueueTimeout)
		defer func() {
			if !enqueueTimer.Stop() {
				select {
				case <-enqueueTimer.C:
				default:
				}
			}
		}()

		select {
		case writeTaskChan <- task:
		case <-enqueueTimer.C:
			logger.LogError(c, kind+" write queue enqueue timeout")
			setCancelReason(cancelReasonWriteEnqueueTimeout)
			return streamWriteResult{}, false
		case <-ctx.Done():
			return streamWriteResult{}, false
		}

		resetTimer(writeTimeoutTimer, writeTimeout)
		select {
		case result := <-resultChan:
			if result.err != nil {
				logger.LogError(c, kind+" data error: "+result.err.Error())
				return result, false
			}
			return result, true
		case <-writeTimeoutTimer.C:
			logger.LogError(c, timeoutMessage)
			setCancelReason(cancelReasonWriteTaskTimeout)
			return streamWriteResult{}, false
		case <-ctx.Done():
			return streamWriteResult{}, false
		}
	}

	defer func() {
		cancel()
		closeRespBody()
		close(writeTaskChan)

		waitTimeout := cleanupWaitTimeout
		if clientDisconnected.Load() {
			waitTimeout = disconnectWaitTimeout
		}

		select {
		case <-writeWorkerDone:
		case <-time.After(waitTimeout):
			logger.LogError(c, "timeout waiting for write worker to exit")
		}

		select {
		case <-scannerDone:
		case <-time.After(waitTimeout):
			logger.LogError(c, "timeout waiting for scanner goroutine to exit")
		}
	}()

	for {
		select {
		case <-streamingTimer.C:
			logger.LogError(c, "streaming timeout")
			return
		case <-ctx.Done():
			if c.Request != nil && c.Request.Context().Err() != nil {
				logger.LogInfo(c, "client disconnected")
				return
			}

			switch cancelReason.Load() {
			case cancelReasonWriteError:
				logger.LogInfo(c, "streaming canceled due to write error")
			case cancelReasonHandlerStop:
				logger.LogInfo(c, "streaming canceled by data handler")
			case cancelReasonWriteEnqueueTimeout:
				logger.LogError(c, "streaming canceled due to write queue enqueue timeout")
			case cancelReasonWriteTaskTimeout:
				logger.LogError(c, "streaming canceled due to write task timeout")
			default:
				logger.LogInfo(c, "streaming canceled")
			}
			return
		case <-pingC:
			_, ok := dispatchWriteTask("ping", "", "ping data send timeout")
			if !ok {
				return
			}
			if common.DebugEnabled {
				println("ping data sent")
			}
		case event, ok := <-scannerEventChan:
			if !ok {
				logger.LogInfo(c, "streaming finished")
				return
			}
			if event.err != nil {
				logger.LogError(c, "scanner error: "+event.err.Error())
				return
			}
			if event.done {
				if common.DebugEnabled {
					println("received [DONE], stopping scanner")
				}
				logger.LogInfo(c, "streaming finished")
				return
			}
			if event.activity {
				resetTimer(streamingTimer, streamingTimeout)
				continue
			}

			resetTimer(streamingTimer, streamingTimeout)
			info.SetFirstResponseTime()
			info.ReceivedResponseCount++

			result, ok := dispatchWriteTask("data", event.data, "data handler timeout")
			if !ok {
				return
			}
			if !result.shouldContinue {
				return
			}
		}
	}
}
