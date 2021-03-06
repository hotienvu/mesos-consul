package mesos

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mantl/mesos-consul/registry"
	"github.com/mantl/mesos-consul/state"

	log "github.com/sirupsen/logrus"
)

// Query the consul agent on the Mesos Master
// to initialize the cache.
//
// All services created by mesos-consul are prefixed
// with service-id-prefix flag, followed by a colon.
//
func (m *Mesos) LoadCache() error {
	log.Debug("Populating cache from Consul")

	mh := m.getLeader()

	return m.Registry.CacheLoad(mh.Ip, m.ServiceIdPrefix)
}

func (m *Mesos) RegisterHosts(s state.State) {
	log.Debug("Running RegisterHosts")

	m.Agents = make(map[string]string)

	// Register slaves
	for _, f := range s.Slaves {
		agent := toIP(f.PID.Host)
		port := toPort(f.PID.Port)

		m.Agents[f.ID] = agent

		m.registerHost(&registry.Service{
			ID:      fmt.Sprintf("%s:%s:%s:%s", m.ServiceIdPrefix, m.ServiceName, f.ID, f.Hostname),
			Name:    m.ServiceName,
			Port:    port,
			Address: agent,
			Agent:   agent,
			Tags:    m.agentTags("agent", "follower"),
			Check: &registry.Check{
				HTTP:     fmt.Sprintf("http://%s:%d/slave(1)/health", agent, port),
				Interval: "10s",
			},
		})
	}

	// Register masters
	mas := m.getMasters()
	for _, ma := range mas {
		var tags []string

		if ma.IsLeader {
			tags = m.agentTags("leader", "master")
		} else {
			tags = m.agentTags("master")
		}
		s := &registry.Service{
			ID:      fmt.Sprintf("%s:%s:%s:%s", m.ServiceIdPrefix, m.ServiceName, ma.Ip, ma.PortString),
			Name:    m.ServiceName,
			Port:    ma.Port,
			Address: ma.Ip,
			Agent:   ma.Ip,
			Tags:    tags,
			Check: &registry.Check{
				HTTP:     fmt.Sprintf("http://%s:%d/master/health", ma.Ip, ma.Port),
				Interval: "10s",
			},
		}

		m.registerHost(s)
	}
}

func (m *Mesos) registerHost(s *registry.Service) {
	h := m.Registry.CacheLookup(s.ID)
	if h != nil {
		log.Infof("Host found. Comparing tags: (%v, %v)", h.Tags, s.Tags)

		if sliceEq(s.Tags, h.Tags) {
			m.Registry.CacheMark(s.ID)

			// Tags are the same. Return
			return
		}

		log.Info("Tags changed. Re-registering")

		// Delete cache entry. It will be re-created below
		m.Registry.CacheDelete(s.ID)
	}

	m.Registry.Register(s)
}

func (m *Mesos) registerTask(t *state.Task, agent string) {
	var tags []string

	registered := false

	tname := cleanName(t.Name, m.Separator, m.SharedServiceName)
	log.Debugf("original TaskName : (%v)", tname)
	if t.Label("overrideTaskName") != "" {
		tname = cleanName(t.Label("overrideTaskName"), m.Separator, m.SharedServiceName)
		log.Debugf("overrideTaskName to : (%v)", tname)
	}
	if !m.TaskPrivilege.Allowed(tname) {
		// Task not allowed to be registered
		return
	}

	address := t.IP(m.IpOrder...)

	// build a map to indicate public ports
	var registerPorts map[int]struct{}
	if m.ServicePortLabel != "" {
		p := t.Label(m.ServicePortLabel)
		if p != "" {
			ps := strings.Split(p, ",")
			if len(ps) > 0 {
				registerPorts = make(map[int]struct{}, 0)
				for _, pv := range ps {
					pv = strings.TrimSpace(pv)
					pi, err := strconv.Atoi(pv)
					if err == nil {
						registerPorts[pi] = struct{}{}
					}
				}
			}
		}
	}

	l := t.Label("tags")
	if l != "" {
		tags = strings.Split(t.Label("tags"), ",")
	} else {
		tags = []string{}
	}

	tags = buildRegisterTaskTags(tname, tags, m.taskTag)

	for key := range t.DiscoveryInfo.Ports.DiscoveryPorts {
		// We append -portN to ports after the first.
		// This is done to preserve compatibility with
		// existing implementations which may rely on the
		// old unprefixed name.
		svcName := tname
		if key > 0 {
			svcName = fmt.Sprintf("%s-port%d", svcName, key+1)
		}
		var porttags []string
		discoveryPort := state.DiscoveryPort(t.DiscoveryInfo.Ports.DiscoveryPorts[key])
		serviceName := discoveryPort.Name
		servicePort := strconv.Itoa(discoveryPort.Number)
		log.Debugf("%+v framework has %+v as a name for %+v port",
			t.Name,
			discoveryPort.Name,
			discoveryPort.Number)
		pl := discoveryPort.Label("tags")
		if pl != "" {
			porttags = strings.Split(discoveryPort.Label("tags"), ",")
		} else {
			porttags = []string{}
		}
		if discoveryPort.Name != "" {
			m.Registry.Register(&registry.Service{
				ID:      fmt.Sprintf("%s:%s:%s:%s:%d", m.ServiceIdPrefix, agent, svcName, address, discoveryPort.Number),
				Name:    svcName,
				Port:    toPort(servicePort),
				Address: address,
				Tags:    append(append(tags, serviceName), porttags...),
				Check: GetCheck(t, &CheckVar{
					Host: toIP(address),
					Port: servicePort,
				}),
				Agent: toIP(agent),
			})
			registered = true
		}
	}

	if t.Resources.PortRanges != "" {
		for key, port := range t.Resources.Ports() {
			// do not register port if explicit port label was found
			if _, ok := registerPorts[key]; len(registerPorts) > 0 && !ok {
				continue
			}

			// We append -portN to ports after the first.
			// This is done to preserve compatibility with
			// existing implementations which may rely on the
			// old unprefixed name.
			svcName := tname
			if key > 0 {
				svcName = fmt.Sprintf("%s-port%d", svcName, key+1)
			}
			m.Registry.Register(&registry.Service{
				ID:      fmt.Sprintf("%s:%s:%s:%s:%s", m.ServiceIdPrefix, agent, svcName, address, port),
				Name:    svcName,
				Port:    toPort(port),
				Address: address,
				Tags:    tags,
				Check: GetCheck(t, &CheckVar{
					Host: toIP(address),
					Port: port,
				}),
				Agent: toIP(agent),
			})
			registered = true
		}
	}

	if !registered {
		m.Registry.Register(&registry.Service{
			ID:      fmt.Sprintf("%s:%s-%s:%s", m.ServiceIdPrefix, agent, tname, address),
			Name:    tname,
			Address: address,
			Tags:    tags,
			Check: GetCheck(t, &CheckVar{
				Host: toIP(address),
			}),
			Agent: toIP(agent),
		})
	}
}

// buildRegisterTaskTags takes a cleaned task name, a slice of starting tags, and the processed
// taskTag map and returns a slice of tags that should be applied to this task.
func buildRegisterTaskTags(taskName string, startingTags []string, taskTag map[string][]string) []string {
	result := startingTags
	tnameLower := strings.ToLower(taskName)

	for pattern, taskTags := range taskTag {
		for _, tag := range taskTags {
			if strings.Contains(tnameLower, pattern) {
				if !sliceContainsString(result, tag) {
					log.WithField("task-tag", tnameLower).Debug("Task matches pattern for tag")
					result = append(result, tag)
				}
			}
		}
	}

	return result
}

func (m *Mesos) agentTags(ts ...string) []string {
	if len(m.ServiceTags) == 0 {
		return ts
	}

	rval := []string{}

	for _, tag := range m.ServiceTags {
		for _, t := range ts {
			rval = append(rval, fmt.Sprintf("%s.%s", t, tag))
		}
	}

	return rval
}
