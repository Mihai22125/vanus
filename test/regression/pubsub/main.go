// Copyright 2023 Linkall Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	v2 "github.com/cloudevents/sdk-go/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	vanus "github.com/linkall-labs/sdk/golang"
	"github.com/linkall-labs/vanus/observability/log"
	"github.com/linkall-labs/vanus/proto/pkg/cloudevents"
	"github.com/linkall-labs/vanus/test/utils"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

var (
	sentSuccess    int64
	sentFailed     int64
	receivedNumber int64

	totalNumber        = int64(1 << 19) // 1024*1024
	parallelism        = 4
	batchSize          = 8
	maximumPayloadSize = 4 * 1024
	receiveEventPort   = 18080
	waitTimeout        = 30 * time.Second
	totalLatency       = int64(0)
	caseName           = "vanus.regression.pubsub"

	eventbus = os.Getenv("EVENTBUS")
	endpoint = os.Getenv("VANUS_GATEWAY")

	mutex   = sync.Mutex{}
	cache   = map[string]*v2.Event{}
	unmatch = map[string]*v2.Event{}
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	client, err := vanus.Connect(&vanus.ClientOptions{Endpoint: endpoint})
	if err != nil {
		panic(err)
	}
	//s, _ := client.Subscriber(nil) // Pull?
	//s.Subscribe()

	go subscribe(ctx, &receiver{}, receiveEventPort)
	utils.PrintTotal(ctx, map[string]*int64{
		"Sending":   &sentSuccess,
		"Receiving": &receivedNumber,
	})

	publishEvents(ctx, client)
	now := time.Now()
	for time.Since(now) < waitTimeout {
		mutex.Lock()
		if len(cache) == 0 {
			mutex.Unlock()
			break
		}
		for k := range unmatch {
			delete(cache, k)
		}
		mutex.Unlock()
		time.Sleep(time.Second)
	}
	cancel()

	if len(cache) != 0 {
		log.Error(ctx, "failed to run pubsub case because of timeout after finished sending", map[string]interface{}{
			"success":  sentSuccess,
			"received": receivedNumber,
			"lost":     len(cache),
		})
		log.Info(ctx, "lost events are below", nil)
		for _, v := range cache {
			println(v.String())
		}
		os.Exit(1)
	}
	log.Info(ctx, "success to run publish and subscribe case", map[string]interface{}{
		"success":     sentSuccess,
		"sent_failed": sentFailed,
		"received":    receivedNumber,
	})
}

func publishEvents(ctx context.Context, client vanus.Client) {
	wg := sync.WaitGroup{}
	start := time.Now()

	for p := 0; p < parallelism; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			publisher := client.Publisher(&vanus.PublishOptions{Eventbus: eventbus})
			ch := utils.CreateEventFactory(ctx, caseName, maximumPayloadSize)
			for atomic.LoadInt64(&sentSuccess) < totalNumber {
				events := make([]*v2.Event, batchSize)
				for n := 0; n < batchSize; n++ {
					events[n] = <-ch
				}
				err := publisher.Publish(context.Background(), events...)
				if err != nil {
					log.Warning(context.Background(), "failed to send events", map[string]interface{}{
						log.KeyError: err,
					})
					atomic.AddInt64(&sentFailed, int64(len(events)))
				} else {
					mutex.Lock()
					for _, e := range events {
						cache[e.ID()] = e
					}
					mutex.Unlock()
					atomic.AddInt64(&sentSuccess, int64(len(events)))
				}
			}
		}()
	}
	wg.Wait()

	log.Info(ctx, "finished to sent all events", map[string]interface{}{
		"success": sentSuccess,
		"failed":  sentFailed,
		"used":    time.Now().Sub(start),
	})
}

func subscribe(ctx context.Context, srv cloudevents.CloudEventsServer, port int) {
	ls, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Error(ctx, "failed to send sending finished signal", map[string]interface{}{
			"success":  sentSuccess,
			"received": receivedNumber,
		})
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	cloudevents.RegisterCloudEventsServer(grpcServer, srv)
	err = grpcServer.Serve(ls)
	if err != nil {
		log.Error(nil, "grpc server occurred an error", map[string]interface{}{
			log.KeyError: err,
		})
		os.Exit(1)
	}
}

type receiver struct {
}

func (r *receiver) Send(ctx context.Context, event *cloudevents.BatchEvent) (*emptypb.Empty, error) {
	mutex.Lock()
	defer mutex.Unlock()
	for _, e := range event.Events.Events {
		ce, exist := cache[e.Id]
		if !exist {
			unmatch[e.Id] = e
			log.Warning(ctx, "received a event, but which isn't found in cache", map[string]interface{}{
				"e": e.String(),
			})
			continue
		}
		if ce.GetTextData() != string(e.GetBinaryData()) {
			log.Error(ctx, "received a event, but data was corrupted", nil)
			os.Exit(1)
		}
		delete(cache, e.Id)
		atomic.AddInt64(&totalLatency, ce.Attributes["time"].GetCeTimestamp().AsTime().UnixMicro())
	}
	atomic.AddInt64(&receivedNumber, int64(len(event.Events.Events)))
	return &emptypb.Empty{}, nil
}
