/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#   Mike Trinkala (trink@mozilla.com)
#
# ***** END LICENSE BLOCK *****/

package pipeline

import (
	"code.google.com/p/go-uuid/uuid"
	"fmt"
	"github.com/bbangert/toml"
	. "github.com/mozilla-services/heka/message"
	"log"
	"os"
	"regexp"
	"sync"
	"time"
)

// Cap size of our decoder set arrays
const MAX_HEADER_MESSAGEENCODING Header_MessageEncoding = 256

var (
	AvailablePlugins         = make(map[string]func() interface{})
	DecodersByEncoding       = make(map[Header_MessageEncoding]string)
	topHeaderMessageEncoding Header_MessageEncoding
	PluginTypeRegex          = regexp.MustCompile("^.*(Decoder|Filter|Input|Output)$")
)

// Adds a plugin to the set of usable Heka plugins that can be referenced from
// a Heka config file.
func RegisterPlugin(name string, factory func() interface{}) {
	AvailablePlugins[name] = factory
}

// Generic plugin configuration type that will be used for plugins that don't
// provide the `HasConfigStruct` interface.
type PluginConfig map[string]toml.Primitive

// API made available to all plugins providing Heka-wide utility functions.
type PluginHelper interface {

	// Returns an `OutputRunner` for an output plugin registered using the
	// specified name, or ok == false if no output by that name is registered.
	Output(name string) (oRunner OutputRunner, ok bool)

	// Returns an `FilterRunner` for a filter plugin registered using the
	// specified name, or ok == false if no filter by that name is registered.
	Filter(name string) (fRunner FilterRunner, ok bool)

	// Returns the currently running Heka instance's unique PipelineConfig
	// object.
	PipelineConfig() *PipelineConfig

	// Returns a single `DecoderSet` of running decoders for use by any plugin
	// (usually inputs) that wants to decode binary data into a `Message`
	// struct.
	DecoderSet() DecoderSet

	// Expects a loop count value from an existing message (or zero if there's
	// no relevant existing message), returns an initialized `PipelinePack`
	// pointer that can be populated w/ message data and inserted into the
	// Heka pipeline. Returns `nil` if the loop count value provided is
	// greater than the maximum allowed by the Heka instance.
	PipelinePack(msgLoopCount uint) *PipelinePack
}

// Indicates a plug-in has a specific-to-itself config struct that should be
// passed in to its Init method.
type HasConfigStruct interface {
	// Returns a default-value-populated configuration structure into which
	// the plugin's TOML configuration will be deserialized.
	ConfigStruct() interface{}
}

// Indicates a plug-in can handle being restart should it exit before
// heka is shut-down.
type Restarting interface {
	// Is called anytime the plug-in returns during the main Run loop to
	// clean up the plug-in state and determine whether the plugin should
	// be restarted or not.
	Cleanup()
}

// Master config object encapsulating the entire heka/pipeline configuration.
type PipelineConfig struct {
	// All running InputRunners, by name.
	InputRunners map[string]InputRunner
	// PluginWrappers that can create Input plugin objects.
	inputWrappers map[string]*PluginWrapper
	// PluginWrappers that can create Decoder plugin objects.
	DecoderWrappers map[string]*PluginWrapper
	// All running FilterRunners, by name.
	FilterRunners map[string]FilterRunner
	// PluginWrappers that can create Filter plugin objects.
	filterWrappers map[string]*PluginWrapper
	// All running OutputRunners, by name.
	OutputRunners map[string]OutputRunner
	// PluginWrappers that can create Output plugin objects.
	outputWrappers map[string]*PluginWrapper
	// Heka message router instance.
	router *messageRouter
	// PipelinePack supply for Input plugins.
	inputRecycleChan chan *PipelinePack
	// PipelinePack supply for Filter plugins (separate pool prevents
	// deadlocks).
	injectRecycleChan chan *PipelinePack
	// Stores log messages generated by plugin config errors.
	logMsgs []string
	// Lock protecting access to the set of running filters so dynamic filters
	// can be safely added and removed whil Heka is running.
	filtersLock sync.Mutex
	// Is freed when all FilterRunners have stopped.
	filtersWg sync.WaitGroup
	// Is freed when all DecoderRunners have stopped.
	decodersWg sync.WaitGroup
	// Channels from which running DecoderRunners can be fetched.
	decoderChannels map[string]chan DecoderRunner
	// Slice providing access to all running DecoderRunners.
	allDecoders []DecoderRunner
	// Name of host on which Heka is running.
	hostname string
	// Heka process id.
	pid int32
}

// Creates and initializes a PipelineConfig object. `nil` value for `globals`
// argument means we should use the default global config values.
func NewPipelineConfig(globals *GlobalConfigStruct) (config *PipelineConfig) {
	config = new(PipelineConfig)
	if globals == nil {
		globals = DefaultGlobals()
	}
	// Replace global `Globals` function w/ one that returns our values.
	Globals = func() *GlobalConfigStruct {
		return globals
	}
	config.InputRunners = make(map[string]InputRunner)
	config.inputWrappers = make(map[string]*PluginWrapper)
	config.DecoderWrappers = make(map[string]*PluginWrapper)
	config.FilterRunners = make(map[string]FilterRunner)
	config.filterWrappers = make(map[string]*PluginWrapper)
	config.OutputRunners = make(map[string]OutputRunner)
	config.outputWrappers = make(map[string]*PluginWrapper)
	config.router = NewMessageRouter()
	config.inputRecycleChan = make(chan *PipelinePack, globals.PoolSize)
	config.injectRecycleChan = make(chan *PipelinePack, globals.PoolSize)
	config.logMsgs = make([]string, 0, 4)
	config.decoderChannels = make(map[string]chan DecoderRunner)
	config.allDecoders = make([]DecoderRunner, 0, 10)
	config.hostname, _ = os.Hostname()
	config.pid = int32(os.Getpid())

	return config
}

// Callers should pass in the msgLoopCount value from any relevant Message
// objects they are holding. Returns a PipelinePack for injection into Heka
// pipeline, or nil if the msgLoopCount is above the configured maximum.
func (self *PipelineConfig) PipelinePack(msgLoopCount uint) *PipelinePack {
	if msgLoopCount++; msgLoopCount > Globals().MaxMsgLoops {
		return nil
	}
	pack := <-self.injectRecycleChan
	pack.Message.SetTimestamp(time.Now().UnixNano())
	pack.Message.SetUuid(uuid.NewRandom())
	pack.Message.SetHostname(self.hostname)
	pack.Message.SetPid(self.pid)
	pack.RefCount = 1
	pack.MsgLoopCount = msgLoopCount
	return pack
}

// Returns OutputRunner registered under the specified name, or nil (and ok ==
// false) if no such name is registered.
func (self *PipelineConfig) Output(name string) (oRunner OutputRunner, ok bool) {
	oRunner, ok = self.OutputRunners[name]
	return
}

// Returns the underlying config object via the Helper interface.
func (self *PipelineConfig) PipelineConfig() *PipelineConfig {
	return self
}

// Returns a DecoderSet which exposes an API for accessing running decoders.
func (self *PipelineConfig) DecoderSet() (ds DecoderSet) {
	ds, _ = newDecoderSet(self.decoderChannels)
	return
}

// Returns a FilterRunner with the given name, or nil and ok == false if no
// such name is registered.
func (self *PipelineConfig) Filter(name string) (fRunner FilterRunner, ok bool) {
	fRunner, ok = self.FilterRunners[name]
	return
}

// Starts the provided FilterRunner and adds it to the set of running Filters.
func (self *PipelineConfig) AddFilterRunner(fRunner FilterRunner) error {
	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	self.FilterRunners[fRunner.Name()] = fRunner
	self.filtersWg.Add(1)
	if err := fRunner.Start(self, &self.filtersWg); err != nil {
		self.filtersWg.Done()
		return fmt.Errorf("AddFilterRunner '%s' failed to start: %s",
			fRunner.Name(), err)
	} else {
		self.router.MrChan() <- fRunner.MatchRunner()
	}
	return nil
}

// Removes the specified FilterRunner from the configuration, returns false if
// no such name is registered.
func (self *PipelineConfig) RemoveFilterRunner(name string) bool {
	if Globals().Stopping {
		return false
	}

	self.filtersLock.Lock()
	defer self.filtersLock.Unlock()
	if fRunner, ok := self.FilterRunners[name]; ok {
		self.router.MrChan() <- fRunner.MatchRunner()
		close(fRunner.InChan())
		delete(self.FilterRunners, name)
		return true
	}
	return false
}

type ConfigFile PluginConfig

// This struct provides a structure for the available retry options for
// a plugin that supports being restarted
type RetryOptions struct {
	// Maximum time in seconds between restart attempts. Defaults to 30s.
	MaxDelay string `toml:"max_delay"`
	// Starting delay in milliseconds between restart attempts. Defaults to
	// 250ms.
	Delay string
	// How many times to attempt starting the plugin before failing. Defaults
	// to -1 (retry forever).
	MaxRetries int `toml:"max_retries"`
}

// The TOML spec for plugin configuration options that will be pulled out  by
// Heka itself for runner configuration before the config is passed to the
// Plugin.Init method.
type PluginGlobals struct {
	Typ      string `toml:"type"`
	Ticker   uint   `toml:"ticker_interval"`
	Encoding string `toml:"encoding_name"`
	Matcher  string `toml:"message_matcher"`
	Signer   string `toml:"message_signer"`
	PoolSize uint   `toml:"pool_size"`
	Retries  RetryOptions
}

// Default Decoders configuration.
var defaultDecoderTOML = `
[JsonDecoder]
encoding_name = "JSON"

[ProtobufDecoder]
encoding_name = "PROTOCOL_BUFFER"
`

// A helper object to support delayed plugin creation.
type PluginWrapper struct {
	name          string
	configCreator func() interface{}
	pluginCreator func() interface{}
}

// Create a new instance of the plugin and return it. Errors are ignored. Call
// CreateWithError if an error is needed.
func (self *PluginWrapper) Create() (plugin interface{}) {
	plugin, _ = self.CreateWithError()
	return
}

// Create a new instance of the plugin and return it, or nil and appropriate
// error value if this isn't possible.
func (self *PluginWrapper) CreateWithError() (plugin interface{}, err error) {
	defer func() {
		// Slight protection against Init call into plugin code.
		if r := recover(); r != nil {
			plugin = nil
			err = fmt.Errorf("'%s' Init() panicked: %s", self.name, r)
		}
	}()

	plugin = self.pluginCreator()
	err = plugin.(Plugin).Init(self.configCreator())
	return
}

// If `configable` supports the `HasConfigStruct` interface this will use said
// interface to fetch a config struct object and populate it w/ the values in
// provided `config`. If not, simply returns `config` unchanged.
func LoadConfigStruct(config toml.Primitive, configable interface{}) (
	configStruct interface{}, err error) {

	// On two lines for scoping reasons.
	hasConfigStruct, ok := configable.(HasConfigStruct)
	if !ok {
		// If we don't have a config struct, change it to a PluginConfig
		configStruct = PluginConfig{}
		if err = toml.PrimitiveDecode(config, configStruct); err != nil {
			configStruct = nil
		}
		return
	}

	defer func() {
		// Slight protection against ConfigStruct call into plugin code.
		if r := recover(); r != nil {
			configStruct = nil
			err = fmt.Errorf("ConfigStruct() panicked: %s", r)
		}
	}()

	configStruct = hasConfigStruct.ConfigStruct()
	if err = toml.PrimitiveDecode(config, configStruct); err != nil {
		configStruct = nil
		err = fmt.Errorf("Can't unmarshal config: %s", err)
	}
	return
}

// Registers a the specified decoder to be used for messages with the
// specified Heka protocol encoding header.
func regDecoderForHeader(decoderName string, encodingName string) (err error) {
	var encoding Header_MessageEncoding
	var ok bool
	if encodingInt32, ok := Header_MessageEncoding_value[encodingName]; !ok {
		err = fmt.Errorf("No Header_MessageEncoding named '%s'", encodingName)
		return
	} else {
		encoding = Header_MessageEncoding(encodingInt32)
	}
	if encoding > MAX_HEADER_MESSAGEENCODING {
		err = fmt.Errorf("Header_MessageEncoding '%s' value '%d' higher than max '%d'",
			encodingName, encoding, MAX_HEADER_MESSAGEENCODING)
		return
	}
	// Be nice to be able to verify that this is actually a decoder.
	if _, ok = AvailablePlugins[decoderName]; !ok {
		err = fmt.Errorf("No decoder named '%s' registered as a plugin", decoderName)
		return
	}
	if encoding > topHeaderMessageEncoding {
		topHeaderMessageEncoding = encoding
	}
	DecodersByEncoding[encoding] = decoderName
	return
}

// Used internally to log and record plugin config loading errors.
func (self *PipelineConfig) log(msg string) {
	self.logMsgs = append(self.logMsgs, msg)
	log.Println(msg)
}

// loadSection must be passed a plugin name and the config for that plugin. It
// will create a PluginWrapper (i.e. a factory). For decoders we store the
// PluginWrappers and create pools of DecoderRunners for each type, stored in
// our decoder channels. For the other plugin types, we create the plugin,
// configure it, then create the appropriate plugin runner.
func (self *PipelineConfig) loadSection(sectionName string,
	configSection toml.Primitive) (errcnt uint) {
	var ok bool
	var err error
	var pluginGlobals PluginGlobals
	var pluginType string

	wrapper := new(PluginWrapper)
	wrapper.name = sectionName

	// Setup default retry policy
	pluginGlobals.Retries = RetryOptions{
		MaxDelay:   "30s",
		Delay:      "250ms",
		MaxRetries: -1,
	}

	if err = toml.PrimitiveDecode(configSection, &pluginGlobals); err != nil {
		self.log(fmt.Sprintf("Unable to decode config for plugin: %s, error: %s",
			wrapper.name, err.Error()))
		errcnt++
		return
	}
	if pluginGlobals.Typ == "" {
		pluginType = sectionName
	} else {
		pluginType = pluginGlobals.Typ
	}

	if wrapper.pluginCreator, ok = AvailablePlugins[pluginType]; !ok {
		self.log(fmt.Sprintf("No such plugin: %s", wrapper.name))
		errcnt++
		return
	}

	// Create plugin, test config object generation.
	plugin := wrapper.pluginCreator()
	var config interface{}
	if config, err = LoadConfigStruct(configSection, plugin); err != nil {
		self.log(fmt.Sprintf("Can't load config for %s '%s': %s", sectionName,
			wrapper.name, err))
		errcnt++
		return
	}
	wrapper.configCreator = func() interface{} { return config }

	// Apply configuration to instantiated plugin.
	configPlugin := func() (err error) {
		defer func() {
			// Slight protection against Init call into plugin code.
			if r := recover(); r != nil {
				err = fmt.Errorf("Init() panicked: %s", r)
			}
		}()
		err = plugin.(Plugin).Init(config)
		return
	}
	if err = configPlugin(); err != nil {
		self.log(fmt.Sprintf("Initialization failed for '%s': %s",
			sectionName, err))
		errcnt++
		return
	}

	// Determine the plugin type
	pluginCats := PluginTypeRegex.FindStringSubmatch(pluginType)
	if len(pluginCats) < 2 {
		self.log(fmt.Sprintf("Type doesn't contain valid plugin name: %s", pluginType))
		errcnt++
		return
	}
	pluginCategory := pluginCats[1]

	// For decoders check to see if we need to register against a protocol
	// header, store the wrapper and continue.
	if pluginCategory == "Decoder" {
		if pluginGlobals.Encoding != "" {
			err = regDecoderForHeader(pluginType, pluginGlobals.Encoding)
			if err != nil {
				self.log(fmt.Sprintf(
					"Can't register decoder '%s' for encoding '%s': %s",
					wrapper.name, pluginGlobals.Encoding, err))
				errcnt++
				return
			}
		}
		self.DecoderWrappers[wrapper.name] = wrapper

		if pluginGlobals.PoolSize == 0 {
			pluginGlobals.PoolSize = uint(Globals().DecoderPoolSize)
		}
		// Creates/starts a DecoderRunner wrapped around the decoder and puts
		// it on the channel.
		makeDRunner := func(name string, decoder Decoder, dChan chan DecoderRunner) {
			dRunner := NewDecoderRunner(name, decoder, &pluginGlobals)
			self.decodersWg.Add(1)
			dRunner.Start(self, &self.decodersWg)
			self.allDecoders = append(self.allDecoders, dRunner)
			dChan <- dRunner
		}
		// First use the decoder we've already created.
		decoderChan := make(chan DecoderRunner, pluginGlobals.PoolSize)
		makeDRunner(fmt.Sprintf("%s-0", wrapper.name), plugin.(Decoder), decoderChan)
		// Then create any add'l ones as needed to get to the specified pool
		// size.
		for i := 1; i < int(pluginGlobals.PoolSize); i++ {
			decoder := wrapper.Create().(Decoder)
			makeDRunner(fmt.Sprintf("%s-%d", wrapper.name, i), decoder, decoderChan)
		}
		self.decoderChannels[wrapper.name] = decoderChan
		return
	}

	// For inputs we just store the InputRunner and we're done.
	if pluginCategory == "Input" {
		self.InputRunners[wrapper.name] = NewInputRunner(wrapper.name,
			plugin.(Input), &pluginGlobals)
		self.inputWrappers[wrapper.name] = wrapper
		return
	}

	// Filters and outputs have a few more config settings.
	runner := NewFORunner(wrapper.name, plugin.(Plugin), &pluginGlobals)
	runner.name = wrapper.name

	if pluginGlobals.Ticker != 0 {
		runner.tickLength = time.Duration(pluginGlobals.Ticker) * time.Second
	}

	var matcher *MatchRunner
	if pluginGlobals.Matcher != "" {
		if matcher, err = NewMatchRunner(pluginGlobals.Matcher,
			pluginGlobals.Signer); err != nil {
			self.log(fmt.Sprintf("Can't create message matcher for '%s': %s",
				wrapper.name, err))
			errcnt++
			return
		}
		runner.matcher = matcher
	}

	switch pluginCategory {
	case "Filter":
		if matcher != nil {
			self.router.fMatchers = append(self.router.fMatchers, matcher)
		}
		self.FilterRunners[runner.name] = runner
		self.filterWrappers[runner.name] = wrapper
	case "Output":
		if matcher != nil {
			self.router.oMatchers = append(self.router.oMatchers, matcher)
		}
		self.OutputRunners[runner.name] = runner
		self.outputWrappers[runner.name] = wrapper
	}

	return
}

// LoadFromConfigFile loads a TOML configuration file and stores the
// result in the value pointed to by config. The maps in the config
// will be initialized as needed.
//
// The PipelineConfig should be already initialized before passed in via
// its Init function.
func (self *PipelineConfig) LoadFromConfigFile(filename string) (err error) {
	var configFile ConfigFile
	if _, err = toml.DecodeFile(filename, &configFile); err != nil {
		return fmt.Errorf("Error decoding config file: %s", err)
	}

	// Load all the plugins
	var errcnt uint
	for name, conf := range configFile {
		log.Println("Loading: ", name)
		errcnt += self.loadSection(name, conf)
	}

	// Add JSON/PROTOCOL_BUFFER decoders if none were configured
	var configDefault ConfigFile
	toml.Decode(defaultDecoderTOML, &configDefault)
	dWrappers := self.DecoderWrappers

	if _, ok := dWrappers["JsonDecoder"]; !ok {
		log.Println("Loading: JsonDecoder")
		errcnt += self.loadSection("JsonDecoder", configDefault["JsonDecoder"])
	}
	if _, ok := dWrappers["ProtobufDecoder"]; !ok {
		log.Println("Loading: ProtobufDecoder")
		errcnt += self.loadSection("ProtobufDecoder", configDefault["ProtobufDecoder"])
	}

	if errcnt != 0 {
		return fmt.Errorf("%d errors loading plugins", errcnt)
	}

	return
}

func init() {
	RegisterPlugin("UdpInput", func() interface{} {
		return new(UdpInput)
	})
	RegisterPlugin("TcpInput", func() interface{} {
		return new(TcpInput)
	})
	RegisterPlugin("JsonDecoder", func() interface{} {
		return new(JsonDecoder)
	})
	RegisterPlugin("ProtobufDecoder", func() interface{} {
		return new(ProtobufDecoder)
	})
	RegisterPlugin("StatsdInput", func() interface{} {
		return new(StatsdInput)
	})
	RegisterPlugin("LogOutput", func() interface{} {
		return new(LogOutput)
	})
	RegisterPlugin("FileOutput", func() interface{} {
		return new(FileOutput)
	})
	RegisterPlugin("WhisperOutput", func() interface{} {
		return new(WhisperOutput)
	})
	RegisterPlugin("LogfileInput", func() interface{} {
		return new(LogfileInput)
	})
	RegisterPlugin("TcpOutput", func() interface{} {
		return new(TcpOutput)
	})
	RegisterPlugin("StatFilter", func() interface{} {
		return new(StatFilter)
	})
	RegisterPlugin("SandboxFilter", func() interface{} {
		return new(SandboxFilter)
	})
	RegisterPlugin("LoglineDecoder", func() interface{} {
		return new(LoglineDecoder)
	})
	RegisterPlugin("CounterFilter", func() interface{} {
		return new(CounterFilter)
	})
	RegisterPlugin("SandboxManagerFilter", func() interface{} {
		return new(SandboxManagerFilter)
	})
	RegisterPlugin("DashboardOutput", func() interface{} {
		return new(DashboardOutput)
	})
}
