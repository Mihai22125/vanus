// Copyright 2022 Linkall Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/linkall-labs/vanus/observability/log"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

var (
	ErrStartEtcd         = errors.New("start etcd failed")
	ErrStartEtcdCanceled = errors.New("etcd start canceled")
)

type Member interface {
	Init(context.Context, Config) error
	Start(context.Context) (<-chan struct{}, error)
	Stop(context.Context)
	RegisterMembershipChangedProcessor(MembershipEventProcessor)
	ResignIfLeader()
	IsLeader() bool
	GetLeaderID() string
	GetLeaderAddr() string
	IsReady() bool
}

func New(topology map[string]string) *member {
	m := &member{
		topology: topology,
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

type EventType string

const (
	EventBecomeLeader   = "leader"
	EventBecomeFollower = "follower"
)

type MembershipChangedEvent struct {
	Type EventType
}

type MembershipEventProcessor func(ctx context.Context, event MembershipChangedEvent) error

type member struct {
	// instance   *embed.Etcd
	cfg           *Config
	ctx           context.Context
	cancel        context.CancelFunc
	client        *clientv3.Client
	resourcelock  string
	leaseDuration int64
	session       *concurrency.Session
	mutex         *concurrency.Mutex
	isLeader      bool
	handlers      []MembershipEventProcessor
	topology      map[string]string
	wg            sync.WaitGroup
	mu            sync.RWMutex
	exit          chan struct{}
	isReady       bool
}

const (
	dialTimeout          = 5
	dialKeepAliveTime    = 1
	dialKeepAliveTimeout = 3
	acquireLockDuration  = 5
)

func (m *member) Init(ctx context.Context, cfg Config) error {
	m.cfg = &cfg
	m.resourcelock = fmt.Sprintf("%s/%s", ResourceLockKeyPrefixInKVStore, cfg.Name)
	m.leaseDuration = cfg.LeaseDuration
	m.exit = make(chan struct{})

	var err error
	m.client, err = clientv3.New(clientv3.Config{
		Endpoints:            cfg.EtcdEndpoints,
		DialTimeout:          dialTimeout * time.Second,
		DialKeepAliveTime:    dialKeepAliveTime * time.Second,
		DialKeepAliveTimeout: dialKeepAliveTimeout * time.Second,
	})
	if err != nil {
		log.Error(context.Background(), "new etcd v3client failed", map[string]interface{}{
			log.KeyError: err,
		})
		panic("new etcd v3client failed")
	}

	m.session, err = concurrency.NewSession(m.client, concurrency.WithTTL(int(m.leaseDuration)))
	if err != nil {
		log.Error(context.Background(), "new session failed", map[string]interface{}{
			log.KeyError: err,
		})
		panic("new session failed")
	}
	m.mutex = concurrency.NewMutex(m.session, m.resourcelock)
	log.Info(context.Background(), "new leaderelection manager", map[string]interface{}{
		"name":           m.cfg.Name,
		"key":            m.resourcelock,
		"lease_duration": m.leaseDuration,
	})
	return nil
}

func (m *member) Start(ctx context.Context) (<-chan struct{}, error) {
	log.Info(ctx, "start leaderelection", nil)

	if err := m.tryLock(ctx); err == nil {
		return nil, nil
	}
	return m.tryAcquireLockLoop(ctx)
}

func (m *member) tryLock(ctx context.Context) error {
	err := m.mutex.TryLock(ctx)
	if err != nil {
		if errors.Is(err, concurrency.ErrLocked) {
			m.isReady = true
			log.Info(ctx, "try acquire lock, already locked in another session", nil)
			return err
		}
		log.Error(ctx, "acquire lock failed", map[string]interface{}{
			log.KeyError: err,
		})
		return err
	}

	log.Info(ctx, "acquired lock", map[string]interface{}{
		"identity":     m.cfg.Name,
		"resourcelock": m.resourcelock,
	})
	m.isLeader = true
	m.isReady = true
	event := MembershipChangedEvent{
		Type: EventBecomeLeader,
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, handler := range m.handlers {
		err = handler(m.ctx, event)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *member) tryAcquireLockLoop(ctx context.Context) (<-chan struct{}, error) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-ctx.Done():
				log.Warning(ctx, "context canceled at try acquire lock loop", nil)
				return
			case <-m.session.Done():
				m.isLeader = false
				event := MembershipChangedEvent{
					Type: EventBecomeFollower,
				}
				m.mu.RLock()
				defer m.mu.RUnlock()
				for _, handler := range m.handlers {
					handler(m.ctx, event)
				}
				close(m.exit)
				return
			default:
				if !m.isLeader {
					if err := m.tryLock(ctx); err == nil {
						close(m.exit)
						return
					}
				}
				time.Sleep(acquireLockDuration * time.Second)
			}
		}
	}()
	return m.exit, nil
}

func (m *member) Stop(ctx context.Context) {
	log.Info(ctx, "stop leaderelection", nil)
	err := m.release(ctx)
	if err != nil {
		log.Error(ctx, "release lock failed", map[string]interface{}{
			log.KeyError: err,
		})
		return
	}
	m.wg.Wait()
}

func (m *member) release(ctx context.Context) error {
	err := m.mutex.Unlock(ctx)
	if err != nil {
		log.Error(ctx, "unlock error", map[string]interface{}{
			log.KeyError: err,
		})
		return err
	}
	err = m.session.Close()
	if err != nil {
		log.Error(ctx, "session close error", map[string]interface{}{
			log.KeyError: err,
		})
		return err
	}
	log.Info(ctx, "released lock", nil)
	return nil
}

func (m *member) RegisterMembershipChangedProcessor(handler MembershipEventProcessor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

func (m *member) ResignIfLeader() {
	// TODO(jiangkai)
}

func (m *member) IsLeader() bool {
	return m.isLeader
}

func (m *member) GetLeaderID() string {
	return os.Getenv("POD_NAME")
}

func (m *member) GetLeaderAddr() string {
	if value, ok := m.topology[os.Getenv("POD_NAME")]; ok {
		return value
	}
	return ""
}

func (m *member) IsReady() bool {
	return m.isReady
}
