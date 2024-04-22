// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package audit

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/eventlogger"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/helper/constants"
	"github.com/hashicorp/vault/internal/observability/event"
	"github.com/hashicorp/vault/sdk/helper/salt"
	"github.com/hashicorp/vault/sdk/logical"
)

const (
	optionElideListResponses = "elide_list_responses"
	optionFallback           = "fallback"
	optionFilter             = "filter"
	optionFormat             = "format"
	optionHMACAccessor       = "hmac_accessor"
	optionLogRaw             = "log_raw"
	optionPrefix             = "prefix"

	TypeFile   = "file"
	TypeSocket = "socket"
	TypeSyslog = "syslog"
)

var _ Backend = (*backend)(nil)

// Factory is the factory function to create an audit backend.
type Factory func(*BackendConfig, HeaderFormatter) (Backend, error)

// Backend interface must be implemented for an audit
// mechanism to be made available. Audit backends can be enabled to
// sink information to different backends such as logs, file, databases,
// or other external services.
type Backend interface {
	// Salter interface must be implemented by anything implementing Backend.
	Salter

	// The PipelineReader interface allows backends to surface information about their
	// nodes for node and pipeline registration.
	event.PipelineReader

	// IsFallback can be used to determine if this audit backend device is intended to
	// be used as a fallback to catch all events that are not written when only using
	// filtered pipelines.
	IsFallback() bool

	// LogTestMessage is used to check an audit backend before adding it
	// permanently. It should attempt to synchronously log the given test
	// message, WITHOUT using the normal Salt (which would require a storage
	// operation on creation).
	LogTestMessage(context.Context, *logical.LogInput) error

	// Reload is called on SIGHUP for supporting backends.
	Reload() error

	// Invalidate is called for path invalidation
	Invalidate(context.Context)
}

// Salter is an interface that provides a way to obtain a Salt for hashing.
type Salter interface {
	// Salt returns a non-nil salt or an error.
	Salt(context.Context) (*salt.Salt, error)
}

// backend represents an audit backend's shared fields across supported devices (file, socket, syslog).
// NOTE: Use newBackend to initialize the backend.
// e.g. within NewFileBackend, NewSocketBackend, NewSyslogBackend.
type backend struct {
	*backendEnt
	name       string
	nodeIDList []eventlogger.NodeID
	nodeMap    map[eventlogger.NodeID]eventlogger.Node
	salt       *atomic.Value
	saltConfig *salt.Config
	saltMutex  sync.RWMutex
	saltView   logical.Storage
}

// newBackend will create the common backend which should be used by supported audit
// backend types (file, socket, syslog) to which they can create and add their sink.
// It handles basic validation of config and creates required pipelines nodes that
// precede the sink node.
func newBackend(headersConfig HeaderFormatter, conf *BackendConfig) (*backend, error) {
	b := &backend{
		backendEnt: newBackendEnt(conf.Config),
		name:       conf.MountPath,
		saltConfig: conf.SaltConfig,
		saltView:   conf.SaltView,
		salt:       new(atomic.Value),
		nodeIDList: []eventlogger.NodeID{},
		nodeMap:    make(map[eventlogger.NodeID]eventlogger.Node),
	}
	// Ensure we are working with the right type by explicitly storing a nil of the right type.
	b.salt.Store((*salt.Salt)(nil))

	if err := b.configureFilterNode(conf.Config[optionFilter]); err != nil {
		return nil, err
	}

	cfg, err := newFormatterConfig(headersConfig, conf.Config)
	if err != nil {
		return nil, err
	}

	if err := b.configureFormatterNode(conf.MountPath, cfg, conf.Logger); err != nil {
		return nil, err
	}

	return b, nil
}

// newFormatterConfig creates the configuration required by a formatter node using the config map supplied to the factory.
func newFormatterConfig(headerFormatter HeaderFormatter, config map[string]string) (formatterConfig, error) {
	if headerFormatter == nil || reflect.ValueOf(headerFormatter).IsNil() {
		return formatterConfig{}, fmt.Errorf("header formatter is required: %w", ErrInvalidParameter)
	}

	var opt []Option

	if format, ok := config[optionFormat]; ok {
		if !IsValidFormat(format) {
			return formatterConfig{}, fmt.Errorf("unsupported %q: %w", optionFormat, ErrExternalOptions)
		}

		opt = append(opt, WithFormat(format))
	}

	// Check if hashing of accessor is disabled
	if hmacAccessorRaw, ok := config[optionHMACAccessor]; ok {
		v, err := strconv.ParseBool(hmacAccessorRaw)
		if err != nil {
			return formatterConfig{}, fmt.Errorf("unable to parse %q: %w", optionHMACAccessor, ErrExternalOptions)
		}
		opt = append(opt, WithHMACAccessor(v))
	}

	// Check if raw logging is enabled
	if raw, ok := config[optionLogRaw]; ok {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return formatterConfig{}, fmt.Errorf("unable to parse %q: %w", optionLogRaw, ErrExternalOptions)
		}
		opt = append(opt, WithRaw(v))
	}

	if elideListResponsesRaw, ok := config[optionElideListResponses]; ok {
		v, err := strconv.ParseBool(elideListResponsesRaw)
		if err != nil {
			return formatterConfig{}, fmt.Errorf("unable to parse %q: %w", optionElideListResponses, ErrExternalOptions)
		}
		opt = append(opt, WithElision(v))
	}

	if prefix, ok := config[optionPrefix]; ok {
		opt = append(opt, WithPrefix(prefix))
	}

	opts, err := getOpts(opt...)
	if err != nil {
		return formatterConfig{}, err
	}

	return formatterConfig{
		headerFormatter:    headerFormatter,
		elideListResponses: opts.withElision,
		hmacAccessor:       opts.withHMACAccessor,
		omitTime:           opts.withOmitTime, // This must be set in code after creation.
		prefix:             opts.withPrefix,
		raw:                opts.withRaw,
		requiredFormat:     opts.withFormat,
	}, nil
}

// configureFormatterNode is used to configure a formatter node and associated ID on the Backend.
func (b *backend) configureFormatterNode(name string, formatConfig formatterConfig, logger hclog.Logger) error {
	formatterNodeID, err := event.GenerateNodeID()
	if err != nil {
		return fmt.Errorf("error generating random NodeID for formatter node: %w: %w", ErrInternal, err)
	}

	formatterNode, err := newEntryFormatter(name, formatConfig, b, logger)
	if err != nil {
		return fmt.Errorf("error creating formatter: %w", err)
	}

	b.nodeIDList = append(b.nodeIDList, formatterNodeID)
	b.nodeMap[formatterNodeID] = formatterNode

	return nil
}

// wrapMetrics takes a sink node and augments it by wrapping it with metrics nodes.
// Metrics can be used to measure time and count.
func (b *backend) wrapMetrics(name string, id eventlogger.NodeID, n eventlogger.Node) error {
	if n.Type() != eventlogger.NodeTypeSink {
		return fmt.Errorf("unable to wrap node with metrics. %q is not a sink node: %w", name, ErrInvalidParameter)
	}

	// Wrap the sink node with metrics middleware
	sinkMetricTimer, err := newSinkMetricTimer(name, n)
	if err != nil {
		return fmt.Errorf("unable to add timing metrics to sink for path %q: %w", name, err)
	}

	sinkMetricCounter, err := event.NewMetricsCounter(name, sinkMetricTimer, b.getMetricLabeler())
	if err != nil {
		return fmt.Errorf("unable to add counting metrics to sink for path %q: %w", name, err)
	}

	b.nodeIDList = append(b.nodeIDList, id)
	b.nodeMap[id] = sinkMetricCounter

	return nil
}

// Salt is used to provide a salt for HMAC'ing data. If the salt is not currently
// loaded from storage, then loading will be attempted to create a new salt, which
// will then be stored and returned on subsequent calls.
// NOTE: If invalidation occurs the salt will likely be cleared, forcing reload
// from storage.
func (b *backend) Salt(ctx context.Context) (*salt.Salt, error) {
	s := b.salt.Load().(*salt.Salt)
	if s != nil {
		return s, nil
	}

	b.saltMutex.Lock()
	defer b.saltMutex.Unlock()

	s = b.salt.Load().(*salt.Salt)
	if s != nil {
		return s, nil
	}

	newSalt, err := salt.NewSalt(ctx, b.saltView, b.saltConfig)
	if err != nil {
		b.salt.Store((*salt.Salt)(nil))
		return nil, err
	}

	b.salt.Store(newSalt)
	return newSalt, nil
}

// EventType returns the event type for the backend.
func (b *backend) EventType() eventlogger.EventType {
	return event.AuditType.AsEventType()
}

// HasFiltering determines if the first node for the pipeline is an eventlogger.NodeTypeFilter.
func (b *backend) HasFiltering() bool {
	if b.nodeMap == nil {
		return false
	}

	return len(b.nodeIDList) > 0 && b.nodeMap[b.nodeIDList[0]].Type() == eventlogger.NodeTypeFilter
}

// Name for this backend, this must correspond to the mount path for the audit device.
func (b *backend) Name() string {
	return b.name
}

// NodeIDs returns the IDs of the nodes, in the order they are required.
func (b *backend) NodeIDs() []eventlogger.NodeID {
	return b.nodeIDList
}

// Nodes returns the nodes which should be used by the event framework to process audit entries.
func (b *backend) Nodes() map[eventlogger.NodeID]eventlogger.Node {
	return b.nodeMap
}

func (b *backend) LogTestMessage(ctx context.Context, input *logical.LogInput) error {
	if len(b.nodeIDList) > 0 {
		return processManual(ctx, input, b.nodeIDList, b.nodeMap)
	}

	return nil
}

func (b *backend) Reload() error {
	for _, n := range b.nodeMap {
		if n.Type() == eventlogger.NodeTypeSink {
			return n.Reopen()
		}
	}

	return nil
}

func (b *backend) Invalidate(_ context.Context) {
	b.saltMutex.Lock()
	defer b.saltMutex.Unlock()
	b.salt.Store((*salt.Salt)(nil))
}

// HasInvalidAuditOptions is used to determine if a non-Enterprise version of Vault
// is being used when supplying options that contain options exclusive to Enterprise.
func HasInvalidAuditOptions(options map[string]string) bool {
	return !constants.IsEnterprise && hasEnterpriseAuditOptions(options)
}

// hasValidEnterpriseAuditOptions is used to check if any of the options supplied
// are only for use in the Enterprise version of Vault.
func hasEnterpriseAuditOptions(options map[string]string) bool {
	enterpriseAuditOptions := []string{
		optionFallback,
		optionFilter,
	}

	for _, o := range enterpriseAuditOptions {
		if _, ok := options[o]; ok {
			return true
		}
	}

	return false
}

// backendComparer describes the methods required for a backend to be comparable.
type backendComparer interface {
	Options(key string) string
	Path() string
	Type() string
}

// IsDistinct checks some basic properties to determine if the proposed backend is
// distinct amongst another (existing) backend, initially by the backend's path.
// Depending on the type of audit backend, it will examine a specific option to
// ensure devices with different paths cannot use the same sink.
func IsDistinct(proposed backendComparer, existing backendComparer) (bool, error) {
	// Examples:
	// existing: 'sql/mysql/', new: 'sql/mysql/' or
	// existing: 'sql/mysql/', new: 'sql/' or
	// existing: 'sql/', new: 'sql/mysql/'
	if strings.EqualFold(proposed.Path(), existing.Path()) ||
		strings.HasPrefix(proposed.Path(), existing.Path()) ||
		strings.HasPrefix(existing.Path(), proposed.Path()) {
		return false, fmt.Errorf("path %q already in use: %w", existing.Path(), ErrExternalOptions)
	}

	// Depending on which audit device type we have, check the relevant option
	// to ensure it doesn't already exist.
	var opt string
	switch proposed.Type() {
	case TypeFile:
		opt = optionFilePath
	case TypeSocket:
		opt = optionAddress
	case TypeSyslog:
		opt = optionFacility
	default:
		// Not an audit entry
		return false, fmt.Errorf("unexpected 'type' for audit: %q: %w", proposed.Type(), ErrInvalidParameter)
	}

	opt1 := proposed.Options(opt)
	opt2 := existing.Options(opt)

	if isMatch := strings.EqualFold(opt1, opt2); isMatch {
		return false, fmt.Errorf("%q %q already in use on device: %q: %w", opt, opt1, existing.Path(), ErrExternalOptions)
	}

	return true, nil
}
