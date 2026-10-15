package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	testDuration           = 10 * time.Second
	idleTimeout            = 3 * time.Second
	downloadLimit    int64 = 50 * 1024 * 1024
	readBufferSize         = 32 * 1024
	minDownloadBytes int64 = 64 * 1024
	minMeasureTime         = 100 * time.Millisecond
	minSpeed               = 0.5
	connectTimeout         = 5 * time.Second
)

type Result struct {
	IP      string
	Speed   float64
	Bytes   int64
	Used    float64
	Latency time.Duration
}

func makeClient(ip string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableCompression: true,
			DialContext: func(
				ctx context.Context, _, _ string,
			) (net.Conn, error) {
				return (&net.Dialer{
					Timeout: connectTimeout,
				}).DialContext(
					ctx, "tcp", net.JoinHostPort(ip, "443"),
				)
			},
		},
		CheckRedirect: func(
			*http.Request, []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}
}

func checkIP(ip, target string) (Result, bool) {
	r := Result{IP: ip}

	// 连通性/延迟阶段固定测试 Cloudflare trace；下载测速仍使用界面传入的 target。
	const latencyTarget = "https://speed.cloudflare.com/cdn-cgi/trace"

	ctx, cancel := context.WithTimeout(
		context.Background(), connectTimeout,
	)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, latencyTarget, nil,
	)
	if err != nil {
		return r, false
	}
	req.Header.Set("User-Agent", "IP-HTTP-Checker/1.0")
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := makeClient(ip, connectTimeout).Do(req)
	r.Latency = time.Since(start)

	if err != nil {
		return r, false
	}
	defer resp.Body.Close()

	// 收到任何 HTTP 响应即视为连通，不因 3xx、4xx 或 5xx 状态码直接判定失败。
	return r, true
}

type readResult struct {
	n   int
	err error
}

func downloadTest(ip, target string) (Result, error) {
	r := Result{IP: ip}

	ctx, cancel := context.WithTimeout(
		context.Background(), testDuration,
	)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, target, nil,
	)
	if err != nil {
		return r, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set(
		"Range",
		fmt.Sprintf("bytes=0-%d", downloadLimit-1),
	)

	resp, err := makeClient(ip, testDuration).Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusPartialContent {
		return r, fmt.Errorf("HTTP 状态码：%d", resp.StatusCode)
	}

	start := time.Now()
	var firstData, lastData time.Time
	var total int64
	buf := make([]byte, readBufferSize)

	for time.Since(start) < testDuration &&
		total < downloadLimit {

		b := buf
		if left := downloadLimit - total; int64(len(b)) > left {
			b = b[:int(left)]
		}

		ch := make(chan readResult, 1)
		go func(b []byte) {
			n, err := resp.Body.Read(b)
			ch <- readResult{n: n, err: err}
		}(b)

		wait := idleTimeout
		if !lastData.IsZero() {
			wait -= time.Since(lastData)
		}
		if wait <= 0 {
			cancel()
			break
		}

		timer := time.NewTimer(wait)

		select {
		case rr := <-ch:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			if rr.n > 0 {
				now := time.Now()
				if firstData.IsZero() {
					firstData = now
				}
				lastData = now
				total += int64(rr.n)
			}

			if rr.err != nil {
				if rr.err != io.EOF && total == 0 {
					return r, rr.err
				}
				goto finished
			}

			if rr.n == 0 {
				time.Sleep(10 * time.Millisecond)
			}

		case <-timer.C:
			cancel()
			goto finished

		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			goto finished
		}
	}

finished:
	if total < minDownloadBytes {
		return r, fmt.Errorf("有效下载数据不足 64 KiB")
	}

	used := lastData.Sub(start).Seconds()
	if used < minMeasureTime.Seconds() {
		used = minMeasureTime.Seconds()
	}

	r.Bytes = total
	r.Used = used
	r.Speed = float64(total) / used / 1024 / 1024
	return r, nil
}

func main() {
	ip := "104.17.12.34"
	target := "https://speed.cloudflare.com/__down?bytes=50000000"

	if len(os.Args) > 1 {
		ip = os.Args[1]
	}
	if len(os.Args) > 2 {
		target = os.Args[2]
	}

	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
		fmt.Fprintln(os.Stderr, "请输入有效 IPv4 地址")
		os.Exit(2)
	}
	u, err := url.ParseRequestURI(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		fmt.Fprintln(os.Stderr, "测速地址必须是完整的 HTTPS URL")
		os.Exit(2)
	}

	fmt.Println("测试 IP：", ip)
	fmt.Println("延迟目标：https://speed.cloudflare.com/cdn-cgi/trace")
	fmt.Println("下载目标：", target)
	fmt.Println("\n===== APK 原延迟检测逻辑 =====")
	latencyResult, connected := checkIP(ip, target)
	fmt.Printf("HTTPS 响应：%v\n", connected)
	if !connected {
		fmt.Println("延迟检测失败：未收到 HTTP 响应")
		os.Exit(1)
	}
	fmt.Printf("延迟：%.2f ms\n", float64(latencyResult.Latency)/float64(time.Millisecond))

	fmt.Println("\n===== APK 原下载测速逻辑 =====")
	result, err := downloadTest(ip, target)
	if err != nil {
		fmt.Println("下载测速失败：", err)
		os.Exit(1)
	}
	fmt.Printf("接收数据：%d bytes (%.2f MiB)\n", result.Bytes, float64(result.Bytes)/1024/1024)
	fmt.Printf("计时：%.3f 秒\n", result.Used)
	fmt.Printf("下载速度：%.2f MiB/s\n", result.Speed)
}
