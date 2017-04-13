package consul

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/client/driver"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// nomadServicePrefix is the first prefix that scopes all Nomad registered
	// services
	nomadServicePrefix = "_nomad"

	// defaultRetryInterval is how quickly to retry syncing services and
	// checks to Consul when an error occurs. Will backoff up to a max.
	defaultRetryInterval = time.Second

	// defaultMaxRetryInterval is the default max retry interval.
	defaultMaxRetryInterval = 30 * time.Second

	// ttlCheckBuffer is the time interval that Nomad can take to report Consul
	// the check result
	ttlCheckBuffer = 31 * time.Second

	// defaultShutdownWait is how long Shutdown() should block waiting for
	// enqueued operations to sync to Consul by default.
	defaultShutdownWait = time.Minute

	// DefaultQueryWaitDuration is the max duration the Consul Agent will
	// spend waiting for a response from a Consul Query.
	DefaultQueryWaitDuration = 2 * time.Second

	// ServiceTagHTTP is the tag assigned to HTTP services
	ServiceTagHTTP = "http"

	// ServiceTagRPC is the tag assigned to RPC services
	ServiceTagRPC = "rpc"

	// ServiceTagSerf is the tag assigned to Serf services
	ServiceTagSerf = "serf"
)

// CatalogAPI is the consul/api.Catalog API used by Nomad.
type CatalogAPI interface {
	Datacenters() ([]string, error)
	Service(service, tag string, q *api.QueryOptions) ([]*api.CatalogService, *api.QueryMeta, error)
}

// AgentAPI is the consul/api.Agent API used by Nomad.
type AgentAPI interface {
	Services() (map[string]*api.AgentService, error)
	Checks() (map[string]*api.AgentCheck, error)
	CheckRegister(check *api.AgentCheckRegistration) error
	CheckDeregister(checkID string) error
	ServiceRegister(service *api.AgentServiceRegistration) error
	ServiceDeregister(serviceID string) error
	UpdateTTL(id, output, status string) error
}

// addrParser is usually the Task.FindHostAndPortFor method for turning a
// portLabel into an address and port.
type addrParser func(portLabel string) (string, int)

// operations are submitted to the main loop via commit() for synchronizing
// with Consul.
type operations struct {
	regServices []*api.AgentServiceRegistration
	regChecks   []*api.AgentCheckRegistration
	scripts     []*scriptCheck

	deregServices []string
	deregChecks   []string
}

// ServiceClient handles task and agent service registration with Consul.
type ServiceClient struct {
	client           AgentAPI
	logger           *log.Logger
	retryInterval    time.Duration
	maxRetryInterval time.Duration

	// exitCh is closed when the main Run loop exits
	exitCh chan struct{}

	// shutdownCh is closed when the client should shutdown
	shutdownCh chan struct{}

	// shutdownWait is how long Shutdown() blocks waiting for the final
	// sync() to finish. Defaults to defaultShutdownWait
	shutdownWait time.Duration

	opCh chan *operations

	services       map[string]*api.AgentServiceRegistration
	checks         map[string]*api.AgentCheckRegistration
	scripts        map[string]*scriptCheck
	runningScripts map[string]*scriptHandle

	// agent services and checks record entries for the agent itself which
	// should be removed on shutdown
	agentServices map[string]struct{}
	agentChecks   map[string]struct{}
	agentLock     sync.Mutex
}

// NewServiceClient creates a new Consul ServiceClient from an existing Consul API
// Client and logger.
func NewServiceClient(consulClient AgentAPI, logger *log.Logger) *ServiceClient {
	return &ServiceClient{
		client:           consulClient,
		logger:           logger,
		retryInterval:    defaultRetryInterval,
		maxRetryInterval: defaultMaxRetryInterval,
		exitCh:           make(chan struct{}),
		shutdownCh:       make(chan struct{}),
		shutdownWait:     defaultShutdownWait,
		opCh:             make(chan *operations, 8),
		services:         make(map[string]*api.AgentServiceRegistration),
		checks:           make(map[string]*api.AgentCheckRegistration),
		scripts:          make(map[string]*scriptCheck),
		runningScripts:   make(map[string]*scriptHandle),
		agentServices:    make(map[string]struct{}),
		agentChecks:      make(map[string]struct{}),
	}
}

// Run the Consul main loop which retries operations against Consul. It should
// be called exactly once.
func (c *ServiceClient) Run() {
	defer close(c.exitCh)
	retryTimer := time.NewTimer(0)
	<-retryTimer.C // disabled by default
	failures := 0
	for {
		select {
		case <-retryTimer.C:
		case <-c.shutdownCh:
		case ops := <-c.opCh:
			c.merge(ops)
		}

		if err := c.sync(); err != nil {
			if failures == 0 {
				c.logger.Printf("[WARN] consul.sync: failed to update services in Consul: %v", err)
			}
			failures++
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			backoff := c.retryInterval * time.Duration(failures)
			if backoff > c.maxRetryInterval {
				backoff = c.maxRetryInterval
			}
			retryTimer.Reset(backoff)
		} else {
			if failures > 0 {
				c.logger.Printf("[INFO] consul.sync: successfully updated services in Consul")
				failures = 0
			}
		}

		select {
		case <-c.shutdownCh:
			// Exit only after sync'ing all outstanding operations
			if len(c.opCh) > 0 {
				for len(c.opCh) > 0 {
					c.merge(<-c.opCh)
				}
				continue
			}
			return
		default:
		}

	}
}

// commit operations and returns false if shutdown signalled before committing.
func (c *ServiceClient) commit(ops *operations) bool {
	select {
	case c.opCh <- ops:
		return true
	case <-c.shutdownCh:
		return false
	}
}

// merge registrations into state map prior to sync'ing with Consul
func (c *ServiceClient) merge(ops *operations) {
	for _, s := range ops.regServices {
		c.services[s.ID] = s
	}
	for _, check := range ops.regChecks {
		c.checks[check.ID] = check
	}
	for _, s := range ops.scripts {
		c.scripts[s.id] = s
	}
	for _, sid := range ops.deregServices {
		delete(c.services, sid)
	}
	for _, cid := range ops.deregChecks {
		if script, ok := c.runningScripts[cid]; ok {
			script.cancel()
			delete(c.scripts, cid)
		}
		delete(c.checks, cid)
	}
}

// sync enqueued operations.
func (c *ServiceClient) sync() error {
	sreg, creg, sdereg, cdereg := 0, 0, 0, 0

	consulServices, err := c.client.Services()
	if err != nil {
		return fmt.Errorf("error querying Consul services: %v", err)
	}

	consulChecks, err := c.client.Checks()
	if err != nil {
		return fmt.Errorf("error querying Consul checks: %v", err)
	}

	// Remove Nomad services in Consul but unknown locally
	for id := range consulServices {
		if _, ok := c.services[id]; ok {
			// Known service, skip
			continue
		}
		if !isNomadService(id) {
			// Not managed by Nomad, skip
			continue
		}
		// Unknown Nomad managed service; kill
		if err := c.client.ServiceDeregister(id); err != nil {
			return err
		}
		sdereg++
	}

	// Add Nomad services missing from Consul
	for id, service := range c.services {
		if _, ok := consulServices[id]; ok {
			// Already in Consul; skipping
			continue
		}
		if err = c.client.ServiceRegister(service); err != nil {
			return err
		}
		sreg++
	}

	// Remove Nomad checks in Consul but unknown locally
	for id, check := range consulChecks {
		if _, ok := c.checks[id]; ok {
			// Known check, skip
			continue
		}
		if !isNomadService(check.ServiceID) {
			// Not managed by Nomad, skip
			continue
		}
		// Unknown Nomad managed check; kill
		if err := c.client.CheckDeregister(id); err != nil {
			return err
		}
		cdereg++
	}

	// Add Nomad checks missing from Consul
	for id, check := range c.checks {
		if _, ok := consulChecks[id]; ok {
			// Already in Consul; skipping
			continue
		}
		if err := c.client.CheckRegister(check); err != nil {
			return err
		}
		creg++

		// Handle starting scripts
		if script, ok := c.scripts[id]; ok {
			// If it's already running, don't run it again
			if _, running := c.runningScripts[id]; running {
				continue
			}
			// Not running, start and store the handle
			c.runningScripts[id] = script.run()
		}
	}

	c.logger.Printf("[DEBUG] consul.sync: registered %d services, %d checks; deregistered %d services, %d checks",
		sreg, creg, sdereg, cdereg)
	return nil
}

// RegisterAgent registers Nomad agents (client or server). The
// Service.PortLabel should be a literal port to be parsed with SplitHostPort.
// Script checks are not supported and will return an error. Registration is
// asynchronous.
//
// Agents will be deregistered when Shutdown is called.
func (c *ServiceClient) RegisterAgent(role string, services []*structs.Service) error {
	ops := operations{}

	for _, service := range services {
		id := makeAgentServiceID(role, service)

		// Unlike tasks, agents don't use port labels. Agent ports are
		// stored directly in the PortLabel.
		host, rawport, err := net.SplitHostPort(service.PortLabel)
		if err != nil {
			return fmt.Errorf("error parsing port label %q from service %q: %v", service.PortLabel, service.Name, err)
		}
		port, err := strconv.Atoi(rawport)
		if err != nil {
			return fmt.Errorf("error parsing port %q from service %q: %v", rawport, service.Name, err)
		}
		serviceReg := &api.AgentServiceRegistration{
			ID:      id,
			Name:    service.Name,
			Tags:    service.Tags,
			Address: host,
			Port:    port,
		}
		ops.regServices = append(ops.regServices, serviceReg)

		for _, check := range service.Checks {
			checkID := createCheckID(id, check)
			if check.Type == structs.ServiceCheckScript {
				return fmt.Errorf("service %q contains invalid check: agent checks do not support scripts", service.Name)
			}
			checkHost, checkPort := serviceReg.Address, serviceReg.Port
			if check.PortLabel != "" {
				// Unlike tasks, agents don't use port labels. Agent ports are
				// stored directly in the PortLabel.
				host, rawport, err := net.SplitHostPort(check.PortLabel)
				if err != nil {
					return fmt.Errorf("error parsing port label %q from check %q: %v", service.PortLabel, check.Name, err)
				}
				port, err := strconv.Atoi(rawport)
				if err != nil {
					return fmt.Errorf("error parsing port %q from check %q: %v", rawport, check.Name, err)
				}
				checkHost, checkPort = host, port
			}
			checkReg, err := createCheckReg(id, checkID, check, checkHost, checkPort)
			if err != nil {
				return fmt.Errorf("failed to add check %q: %v", check.Name, err)
			}
			ops.regChecks = append(ops.regChecks, checkReg)
		}
	}

	// Now add them to the registration queue
	if ok := c.commit(&ops); !ok {
		// shutting down, exit
		return nil
	}

	// Record IDs for deregistering on shutdown
	c.agentLock.Lock()
	for _, id := range ops.regServices {
		c.agentServices[id.ID] = struct{}{}
	}
	for _, id := range ops.regChecks {
		c.agentChecks[id.ID] = struct{}{}
	}
	c.agentLock.Unlock()
	return nil
}

// serviceRegs creates service registrations, check registrations, and script
// checks from a service.
func (c *ServiceClient) serviceRegs(ops *operations, allocID string, service *structs.Service,
	exec driver.ScriptExecutor, task *structs.Task) error {

	id := makeTaskServiceID(allocID, task.Name, service)
	host, port := task.FindHostAndPortFor(service.PortLabel)
	serviceReg := &api.AgentServiceRegistration{
		ID:      id,
		Name:    service.Name,
		Tags:    make([]string, len(service.Tags)),
		Address: host,
		Port:    port,
	}
	// copy isn't strictly necessary but can avoid bugs especially
	// with tests that may reuse Tasks
	copy(serviceReg.Tags, service.Tags)
	ops.regServices = append(ops.regServices, serviceReg)

	for _, check := range service.Checks {
		checkID := createCheckID(id, check)
		if check.Type == structs.ServiceCheckScript {
			if exec == nil {
				return fmt.Errorf("driver doesn't support script checks")
			}
			ops.scripts = append(ops.scripts, newScriptCheck(
				allocID, task.Name, checkID, check, exec, c.client, c.logger, c.shutdownCh))

		}
		host, port := serviceReg.Address, serviceReg.Port
		if check.PortLabel != "" {
			host, port = task.FindHostAndPortFor(check.PortLabel)
		}
		checkReg, err := createCheckReg(id, checkID, check, host, port)
		if err != nil {
			return fmt.Errorf("failed to add check %q: %v", check.Name, err)
		}
		ops.regChecks = append(ops.regChecks, checkReg)
	}
	return nil
}

// RegisterTask with Consul. Adds all sevice entries and checks to Consul. If
// exec is nil and a script check exists an error is returned.
//
// Actual communication with Consul is done asynchrously (see Run).
func (c *ServiceClient) RegisterTask(allocID string, task *structs.Task, exec driver.ScriptExecutor) error {
	ops := &operations{}
	for _, service := range task.Services {
		if err := c.serviceRegs(ops, allocID, service, exec, task); err != nil {
			return err
		}
	}
	c.commit(ops)
	return nil
}

// UpdateTask in Consul. Does not alter the service if only checks have
// changed.
func (c *ServiceClient) UpdateTask(allocID string, existing, newTask *structs.Task, exec driver.ScriptExecutor) error {
	ops := &operations{}

	existingIDs := make(map[string]*structs.Service, len(existing.Services))
	for _, s := range existing.Services {
		existingIDs[makeTaskServiceID(allocID, existing.Name, s)] = s
	}
	newIDs := make(map[string]*structs.Service, len(newTask.Services))
	for _, s := range newTask.Services {
		newIDs[makeTaskServiceID(allocID, newTask.Name, s)] = s
	}

	parseAddr := newTask.FindHostAndPortFor

	// Loop over existing Service IDs to see if they have been removed or
	// updated.
	for existingID, existingSvc := range existingIDs {
		newSvc, ok := newIDs[existingID]
		if !ok {
			// Existing sevice entry removed
			ops.deregServices = append(ops.deregServices, existingID)
			for _, check := range existingSvc.Checks {
				ops.deregChecks = append(ops.deregChecks, createCheckID(existingID, check))
			}
			continue
		}

		// Service exists and wasn't updated, don't add it later
		delete(newIDs, existingID)

		// Check to see what checks were updated
		existingChecks := make(map[string]struct{}, len(existingSvc.Checks))
		for _, check := range existingSvc.Checks {
			existingChecks[createCheckID(existingID, check)] = struct{}{}
		}

		// Register new checks
		for _, check := range newSvc.Checks {
			checkID := createCheckID(existingID, check)
			if _, exists := existingChecks[checkID]; exists {
				// Check already exists; skip it
				delete(existingChecks, checkID)
				continue
			}

			// New check, register it
			if check.Type == structs.ServiceCheckScript {
				if exec == nil {
					return fmt.Errorf("driver doesn't support script checks")
				}
				ops.scripts = append(ops.scripts, newScriptCheck(
					existingID, newTask.Name, checkID, check, exec, c.client, c.logger, c.shutdownCh))
			}
			host, port := parseAddr(existingSvc.PortLabel)
			if check.PortLabel != "" {
				host, port = parseAddr(check.PortLabel)
			}
			checkReg, err := createCheckReg(existingID, checkID, check, host, port)
			if err != nil {
				return err
			}
			ops.regChecks = append(ops.regChecks, checkReg)
		}

		// Remove existing checks not in updated service
		for cid := range existingChecks {
			ops.deregChecks = append(ops.deregChecks, cid)
		}
	}

	// Any remaining services should just be enqueued directly
	for _, newSvc := range newIDs {
		err := c.serviceRegs(ops, allocID, newSvc, exec, newTask)
		if err != nil {
			return err
		}
	}

	c.commit(ops)
	return nil
}

// RemoveTask from Consul. Removes all service entries and checks.
//
// Actual communication with Consul is done asynchrously (see Run).
func (c *ServiceClient) RemoveTask(allocID string, task *structs.Task) {
	ops := operations{}

	for _, service := range task.Services {
		id := makeTaskServiceID(allocID, task.Name, service)
		ops.deregServices = append(ops.deregServices, id)

		for _, check := range service.Checks {
			ops.deregChecks = append(ops.deregChecks, createCheckID(id, check))
		}
	}

	// Now add them to the deregistration fields; main Run loop will update
	c.commit(&ops)
}

// Shutdown the Consul client. Update running task registations and deregister
// agent from Consul. Blocks up to shutdownWait before giving up on syncing
// operations.
func (c *ServiceClient) Shutdown() error {
	select {
	case <-c.shutdownCh:
		return nil
	default:
	}

	// First deregister Nomad agent Consul entries
	ops := operations{}
	c.agentLock.Lock()
	for id := range c.agentServices {
		ops.deregServices = append(ops.deregServices, id)
	}
	for id := range c.agentChecks {
		ops.deregChecks = append(ops.deregChecks, id)
	}
	c.agentLock.Unlock()
	c.commit(&ops)

	// Then signal shutdown
	close(c.shutdownCh)

	// Give run loop time to sync, but don't block indefinitely
	deadline := time.After(c.shutdownWait)

	// Wait for Run to finish any outstanding operations and exit
	select {
	case <-c.exitCh:
	case <-deadline:
		// Don't wait forever though
		return fmt.Errorf("timed out waiting for Consul operations to complete")
	}

	// Give script checks time to exit (no need to lock as Run() has exited)
	for _, h := range c.runningScripts {
		select {
		case <-h.wait():
		case <-deadline:
			return fmt.Errorf("timed out waiting for script checks to run")
		}
	}
	return nil
}

// makeAgentServiceID creates a unique ID for identifying an agent service in
// Consul.
//
// Agent service IDs are of the form:
//
//	{nomadServicePrefix}-{ROLE}-{Service.Name}-{Service.Tags...}
//	Example Server ID: _nomad-server-nomad-serf
//	Example Client ID: _nomad-client-nomad-client-http
//
func makeAgentServiceID(role string, service *structs.Service) string {
	parts := make([]string, len(service.Tags)+3)
	parts[0] = nomadServicePrefix
	parts[1] = role
	parts[2] = service.Name
	copy(parts[3:], service.Tags)
	return strings.Join(parts, "-")
}

// makeTaskServiceID creates a unique ID for identifying a task service in
// Consul.
//
// Task service IDs are of the form:
//
//	{nomadServicePrefix}-executor-{ALLOC_ID}-{Service.Name}-{Service.Tags...}
//	Example Service ID: _nomad-executor-1234-echo-http-tag1-tag2-tag3
//
func makeTaskServiceID(allocID, taskName string, service *structs.Service) string {
	parts := make([]string, len(service.Tags)+5)
	parts[0] = nomadServicePrefix
	parts[1] = "executor"
	parts[2] = allocID
	parts[3] = taskName
	parts[4] = service.Name
	copy(parts[5:], service.Tags)
	return strings.Join(parts, "-")
}

// createCheckID creates a unique ID for a check.
func createCheckID(serviceID string, check *structs.ServiceCheck) string {
	return check.Hash(serviceID)
}

// createCheckReg creates a Check that can be registered with Consul.
//
// Script checks simply have a TTL set and the caller is responsible for
// running the script and heartbeating.
func createCheckReg(serviceID, checkID string, check *structs.ServiceCheck, host string, port int) (*api.AgentCheckRegistration, error) {
	chkReg := api.AgentCheckRegistration{
		ID:        checkID,
		Name:      check.Name,
		ServiceID: serviceID,
	}
	chkReg.Status = check.InitialStatus
	chkReg.Timeout = check.Timeout.String()
	chkReg.Interval = check.Interval.String()

	switch check.Type {
	case structs.ServiceCheckHTTP:
		if check.Protocol == "" {
			check.Protocol = "http"
		}
		base := url.URL{
			Scheme: check.Protocol,
			Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		}
		relative, err := url.Parse(check.Path)
		if err != nil {
			return nil, err
		}
		url := base.ResolveReference(relative)
		chkReg.HTTP = url.String()
	case structs.ServiceCheckTCP:
		chkReg.TCP = net.JoinHostPort(host, strconv.Itoa(port))
	case structs.ServiceCheckScript:
		chkReg.TTL = (check.Interval + ttlCheckBuffer).String()
	default:
		return nil, fmt.Errorf("check type %+q not valid", check.Type)
	}
	return &chkReg, nil
}

// isNomadService returns true if the ID matches the pattern of a Nomad managed
// service.
func isNomadService(id string) bool {
	return strings.HasPrefix(id, nomadServicePrefix)
}
