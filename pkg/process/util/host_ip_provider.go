package util

import (
	"fmt"
	"time"

	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/util/cache"
	"github.com/DataDog/datadog-agent/pkg/util/ec2"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// GetDockerHostIP returns the IP address of the host. This is meant to be called
// only when the agent is running in a dockerized environment.
func GetDockerHostIP() []string {
	cacheKey := cache.BuildAgentKey("hostIPs")
	if cachedIPs, found := cache.Cache.Get(cacheKey); found {
		return cachedIPs.([]string)
	}

	ips := getDockerHostIPUncached()
	if len(ips) == 0 {
		log.Warnf("could not get host IP")
	}
	cache.Cache.Set(cacheKey, ips, time.Hour*2)
	return ips
}

func getDockerHostIPUncached() []string {
	for _, attempt := range []struct {
		name     string
		provider func() ([]string, error)
	}{
		{"config", getHostIPFFromConfig},
		{"ec2 metadata api", ec2.GetLocalIPv4},
	} {
		log.Debugf("attempting to get host ip from source: %s", attempt.name)
		ips, err := attempt.provider()
		if err != nil {
			log.Debugf("could not deduce host IP from source: %s", err)
		} else {
			return ips
		}
	}
	return nil
}

func getHostIPFFromConfig() ([]string, error) {
	hostIPs := config.Datadog.GetStringSlice("process_agent_config.host_ips")
	if len(hostIPs) == 0 {
		return nil, fmt.Errorf("no host IPs configured")
	}
	return hostIPs, nil
}
