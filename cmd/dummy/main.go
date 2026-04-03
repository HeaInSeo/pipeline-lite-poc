//go:build redis

// cmd/dummy: canonical specimen v0.1 을 N회 Redis에 제출하는 one-shot submitter.
//
// 역할: dummy app은 run을 생성하고 XADD한 뒤 종료한다.
// 역할이 아닌 것: run 결과를 기다리거나, polling하거나, K8s Job을 관찰하지 않는다.
//
// 경계:
//   - dummy(producer)  : run ID 생성 → Redis XADD → 종료
//   - ingress(consumer): XREADGROUP → RunGate → dag-go → K8s Job → XACK
//
// dummy가 --pipeline 필드를 payload에 담아 넘기더라도,
// 실제 파이프라인 선택은 Dispatcher(cmd/ingress --pipeline) 쪽 책임이다.
// dummy는 "무엇을 몇 번 넣는가"만 책임진다.
//
// 사용법:
//
//	# specimen 5개 제출 (기본)
//	go run -tags redis ./cmd/dummy/ -n 5
//
//	# 제출 간 500ms 대기
//	go run -tags redis ./cmd/dummy/ -n 5 -delay 500ms
//
//	# XLEN 확인 (Dispatcher 없이)
//	podman exec poc-redis redis-cli XLEN poc:runs
//
//	# Dispatcher 실행 (별도 터미널)
//	go run -tags redis ./cmd/ingress/ --pipeline specimen
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/seoyhaein/poc/pkg/queue"
)

const (
	redisAddr = "localhost:6379"
	streamKey = "poc:runs"
	groupName = "poc-workers"
	// dummy submitter 전용 consumer ID. Consume을 호출하지 않으므로 실질 영향 없음.
	consumerID = "dummy-submitter"
)

func main() {
	var (
		n        int
		delay    time.Duration
		pipeline string
	)
	flag.IntVar(&n, "n", 1,
		"제출할 run 수")
	flag.DurationVar(&delay, "delay", 0,
		"제출 간 sleep (예: 500ms, 1s)")
	flag.StringVar(&pipeline, "pipeline", "specimen",
		"run payload에 기록할 pipeline 이름 (현재 specimen만 실질 지원)")
	flag.Parse()

	if n < 1 {
		log.Fatalf("[dummy] -n must be >= 1, got %d", n)
	}

	ctx := context.Background()

	q, err := queue.NewRedisRunQueue(ctx, redisAddr, streamKey, groupName, consumerID)
	if err != nil {
		log.Fatalf("[dummy] redis: %v", err)
	}
	defer q.Close()
	log.Printf("[dummy] redis ready  addr=%s stream=%s", redisAddr, streamKey)
	log.Printf("[dummy] submitting   n=%d delay=%s pipeline=%s", n, delay, pipeline)

	// runID prefix timestamp: 동일 세션 내 모든 run에 공통 부분을 공유한다.
	// 세션 타임스탬프(초 단위) + 3자리 시퀀스 → burst에서도 충돌 없음 보장.
	ts := time.Now().UTC().Format("20060102-150405")
	prefix := fmt.Sprintf("dummy-%s", ts)

	var submitted int
	for i := 1; i <= n; i++ {
		runID := fmt.Sprintf("%s-%03d", prefix, i)

		entryID, err := q.Enqueue(ctx, runID)
		if err != nil {
			log.Printf("[dummy] FAILED  seq=%d runID=%s err=%v", i, runID, err)
			continue
		}
		log.Printf("[dummy] enqueued  seq=%03d runID=%s entryID=%s", i, runID, entryID)
		submitted++

		if delay > 0 && i < n {
			time.Sleep(delay)
		}
	}

	log.Printf("[dummy] done  submitted=%d/%d pipeline=%s", submitted, n, pipeline)
}
