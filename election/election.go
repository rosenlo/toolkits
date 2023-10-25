package election

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rosenlo/toolkits/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const PodIp = "POD_IP"

type Config struct {
	Id                string
	Kubeconfig        string
	ElectionName      string
	ElectionNamespace string
}

type LeaderData struct {
	sync.RWMutex
	Name    string
	isValid atomic.Bool
}

func (l *LeaderData) SetLeader(name string) {
	l.Lock()
	l.Name = name
	l.Unlock()
}

func (l *LeaderData) GetLeader() string {
	l.RLock()
	name := l.Name
	l.RUnlock()
	return name
}

func (l *LeaderData) setValid() {
	l.isValid.Store(true)
}

func (l *LeaderData) getValid() bool {
	return l.isValid.Load()
}

var leaderData = &LeaderData{}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func checkLeaderValid(ctx context.Context, lock *resourcelock.LeaseLock) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var observedTime metav1.Time
	for {
		record, _, err := lock.Get(ctx)
		if err == nil {
			observedTime = record.RenewTime
			break
		} else {
			log.Warn(err.Error())
			time.Sleep(5 * time.Second)
		}
	}

	for {
		select {
		case <-ticker.C:
			record, _, err := lock.Get(ctx)
			if err != nil {
				log.Warnf("error: %v", err)
				continue
			}
			if !record.RenewTime.Equal(&observedTime) {
				if len(record.HolderIdentity) == 0 {
					continue
				}
				leaderData.setValid()
				leaderData.SetLeader(record.HolderIdentity)
				log.Infof("check leader finish, leader is %s", record.HolderIdentity)
				return
			} else {
				log.Warnf("leader(%v) validity has expired", record)
			}
		}
	}
}

func getCurrentLeader(ctx context.Context, lock *resourcelock.LeaseLock) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	record, _, err := lock.Get(ctx)
	if err != nil {
		log.Warnf(err.Error())
		return ""
	}

	return record.HolderIdentity
}

func GetLeader() string {
	return leaderData.GetLeader()
}

func IsMaster() (bool, string, error) {
	hostIP := os.Getenv(PodIp)
	if len(hostIP) == 0 {
		log.Warnf("pod_ip is null")
		return false, "", fmt.Errorf("pod_ip is null")
	}
	leaderIP := GetLeader()
	if leaderIP != hostIP {
		return false, leaderIP, nil
	}

	return true, leaderIP, nil
}

func Start(ctx context.Context, cfg *Config, leaderCallback chan struct{}) {
	log.Infof("election id is %s", cfg.Id)
	// leader election uses the Kubernetes API by writing to a
	// lock object, which can be a LeaseLock object (preferred),
	// a ConfigMap, or an Endpoints (deprecated) object.
	// Conflicting writes are detected and each client handles those actions
	// independently.
	config, err := buildConfig(cfg.Kubeconfig)
	if err != nil {
		panic(err)
	}

	client := clientset.NewForConfigOrDie(config)

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      cfg.ElectionName,
			Namespace: cfg.ElectionNamespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Id,
		},
	}

	go checkLeaderValid(ctx, lock)

	// start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// IMPORTANT: you MUST ensure that any code you have that
		// is protected by the lease must terminate **before**
		// you call cancel. Otherwise, you could have a background
		// loop still running and another process could
		// get elected before your background loop finished, violating
		// the stated goal of the lease.
		ReleaseOnCancel: true,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// we're notified when we start - this is where you would
				// usually put your code
				log.Infof("%s is the leader", cfg.Id)
				leaderData.SetLeader(cfg.Id)
				leaderCallback <- struct{}{}
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Infof("leader lost: %s", cfg.Id)
				leaderData.SetLeader(getCurrentLeader(ctx, lock))
			},
			OnNewLeader: func(identity string) {
				if leaderData.getValid() {
					leaderData.SetLeader(identity)
					// we're notified when new leader elected
					log.Infof("new leader elected: %s", identity)
					if identity != cfg.Id {
						leaderCallback <- struct{}{}
					}
				}
			},
		},
	})
}
