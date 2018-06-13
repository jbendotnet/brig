// Package config implements a very opinionated config utility.  It relies on a
// "default spec", i.e. a structure that defines all existing configuration
// keys, their types and their initial default values.  This is used as
// fallback and source of validation. The idea is similar to python's configobj
// (albeit much smaller). Surprisingly I didn't find any similar library in Go.
//
// Note that passing invalid keys to a few methods will cause a panic - on purpose.
// Using a wrong config key is seen as a bug and should be corrected immediately.
// This allows this package to skip error handling on Get() and Set() entirely.
// Also note that I'm not particularly proud of some parts of this code.
//
// In short: This config  does a few things different than the ones I saw for Go.
// Instead of providing numerous possible sources and formats to save your config
// it simply relies on YAML. The focus is not on ultimate convinience but on:
//
// - Providing meaningful validation and default values.
// - Providing built-in documentation for all config values.
// - Making it able to react on changed config values.
// - Being usable from several go routines.
// - In future: Provide an easy way to migrate configs.
package config

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	e "github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

// EnumValidator checks if the supplied string value is in the `options` list.
func EnumValidator(options []string) func(val interface{}) error {
	return func(val interface{}) error {
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("enum value is not a string: %v", val)
		}

		for _, option := range options {
			if option == s {
				return nil
			}
		}

		return fmt.Errorf("not a valid enum value: %v (allowed: %v)", s, options)
	}
}

// IntRangeValidator checks if the supplied integer value lies in the
// inclusive boundaries of `min` and `max`.
func IntRangeValidator(min, max int64) func(val interface{}) error {
	return func(val interface{}) error {
		i, ok := val.(int64)
		if !ok {
			return fmt.Errorf("value is not an integer: %v", val)
		}

		if i < min {
			return fmt.Errorf("value may not be less than %d", min)
		}

		if i > max {
			return fmt.Errorf("value may not be more than %d", max)
		}

		return nil
	}
}

// DefaultEntry represents the metadata for a default value in the config.
// Every possible key has to have a DefaultEntry.
type DefaultEntry struct {
	// Default is the fallback value for this config key.
	// The confg type will be inferred from its literal type.
	Default interface{}

	// NeedsRestart indicates that we need to restart the daemon
	// to have an effect here.
	NeedsRestart bool

	// Docs describes the meaning of the configuration value.
	Docs string

	// Function that can be used to check
	Validator func(val interface{}) error
}

// DefaultMapping is a container to hold all required DefaultEntries.
// It is a nested map with sections as string keys.
type DefaultMapping map[interface{}]interface{}

var (
	typeIntPattern   = regexp.MustCompile(`u{0,1}int(64|32|16|8|)`)
	typeFloatPattern = regexp.MustCompile(`float(32|64|)`)
)

func getDefaultByKeys(keys []string, defaults DefaultMapping) *DefaultEntry {
	if len(keys) == 0 {
		return nil
	}

	child, ok := defaults[keys[0]]
	if !ok {
		return nil
	}

	defaultEntry, ok := child.(DefaultEntry)
	if ok {
		if len(keys) > 1 {
			return nil
		}

		// scalar type, return immediately.
		return &defaultEntry
	}

	section, ok := child.(DefaultMapping)
	if !ok {
		panic(fmt.Errorf("got bad type in default table: %T", child))
	}

	return getDefaultByKeys(keys[1:], section)
}

func getDefaultByKey(key string, defaults DefaultMapping) *DefaultEntry {
	return getDefaultByKeys(strings.Split(key, "."), defaults)
}

func getTypeOf(val interface{}) string {
	typ := reflect.TypeOf(val)
	if typ == nil {
		return ""
	}

	return typ.Name()
}

func isCompatibleType(typeA, typeB string) bool {
	// Be a bit more tolerant regarding integer values.
	if typeIntPattern.MatchString(typeA) {
		return typeIntPattern.MatchString(typeB)
	}

	if typeFloatPattern.MatchString(typeA) {
		return typeFloatPattern.MatchString(typeB)
	}

	return typeA == typeB
}

func keys(root map[interface{}]interface{}, prefix []string, fn func(section map[interface{}]interface{}, key []string) error) error {
	for keyVal := range root {
		key, ok := keyVal.(string)
		if !ok {
			return fmt.Errorf("config contains non string keys: %v", keyVal)
		}

		// Create the next prefix for the next call or the validation check.
		nextPrefix := make([]string, len(prefix), len(prefix)+1)
		copy(nextPrefix, prefix)
		nextPrefix = append(nextPrefix, key)

		child := root[key]
		section, ok := child.(map[interface{}]interface{})
		if ok {
			// It's another sub section we have to visit.
			if err := keys(section, nextPrefix, fn); err != nil {
				return err
			}

			continue
		}

		if err := fn(root, nextPrefix); err != nil {
			return err
		}
	}

	return nil
}

func mergeDefaults(base map[interface{}]interface{}, overlay DefaultMapping) error {
	for keyVal := range overlay {
		key, ok := keyVal.(string)
		if !ok {
			return fmt.Errorf("config contains non string keys: %v", keyVal)
		}

		switch overlayChild := overlay[key].(type) {
		case DefaultMapping:
			baseSection, ok := base[key].(map[interface{}]interface{})
			if !ok {
				baseSection = make(map[interface{}]interface{})
				base[key] = baseSection
			}

			if err := mergeDefaults(baseSection, overlayChild); err != nil {
				return err
			}
		case DefaultEntry:
			if _, ok := base[key]; !ok {
				base[key] = overlayChild.Default
			}
		}
	}

	return nil
}

func validationChecker(root map[interface{}]interface{}, defaults DefaultMapping, prefix []string) error {
	err := keys(root, nil, func(section map[interface{}]interface{}, key []string) error {
		// It's a scalar key. Let's run some diagnostics.
		lastKey := key[len(key)-1]
		child := section[lastKey]

		fullKey := strings.Join(key, ".")
		defaultEntry := getDefaultByKey(fullKey, defaults)
		if defaultEntry == nil {
			return fmt.Errorf("no default for key: %v", fullKey)
		}

		defType := getTypeOf(defaultEntry.Default)
		if defType == "" {
			return fmt.Errorf("no default found for key `%v`", fullKey)
		}

		valType := getTypeOf(child)
		if !isCompatibleType(valType, defType) {
			return fmt.Errorf(
				"type mismatch: want `%v`, got `%v` for key `%v`",
				defType,
				valType,
				fullKey,
			)
		}

		// Handle a few special cases here that come from go's type system.
		// Doing something like this will lead to a panic:
		//
		//     interface{}(int(42)).(int64)
		//
		// Since this is a config we do not care very much for extremely
		// big numbers and can therefore convert all numbers to int64.
		// The code below does that + something similar for float{32,64}.

		if typeIntPattern.MatchString(valType) {
			destType := reflect.TypeOf(int64(0))
			section[lastKey] = reflect.ValueOf(child).Convert(destType).Int()
		}

		if typeFloatPattern.MatchString(valType) {
			destType := reflect.TypeOf(float64(0))
			section[lastKey] = reflect.ValueOf(child).Convert(destType).Float()
		}

		// Do user defined validation:
		if defaultEntry.Validator != nil {
			if err := defaultEntry.Validator(section[lastKey]); err != nil {
				return err
			}
		}

		// Valid key.
		return nil
	})

	if err != nil {
		return err
	}

	// Fill in keys that are not present in the passed config:
	return mergeDefaults(root, defaults)
}

////////////

type callback struct {
	fn  func(key string)
	key string
}

// Config s a helper that built is around a YAML file.
// It supports typed gets and sets, change notifications and
// basic validation with defaults.
type Config struct {
	mu *sync.Mutex

	section         string
	defaults        DefaultMapping
	memory          map[interface{}]interface{}
	callbackCount   int
	changeCallbacks map[string]map[int]callback
}

func prefixKey(section, key string) string {
	if section == "" {
		return key
	}

	return strings.Trim(section, ".") + "." + strings.Trim(key, ".")
}

// Open creates a new config from the data in `r`.
// The mapping in `defaults ` tells the config which keys to expect
// and what type each of it should have.
func Open(r io.Reader, defaults DefaultMapping) (*Config, error) {
	if defaults == nil {
		return nil, fmt.Errorf("need a default mapping")
	}

	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	memory := make(map[interface{}]interface{})
	if err := yaml.Unmarshal(data, memory); err != nil {
		return nil, err
	}

	if err := validationChecker(memory, defaults, []string{}); err != nil {
		return nil, e.Wrapf(err, "validate")
	}

	return &Config{
		mu:              &sync.Mutex{},
		defaults:        defaults,
		memory:          memory,
		changeCallbacks: make(map[string]map[int]callback),
	}, nil
}

// Save will write a YAML representation of the current config to `w`.
func (cfg *Config) Save(w io.Writer) error {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	data, err := yaml.Marshal(cfg.memory)
	if err != nil {
		return err
	}

	if _, err := w.Write(data); err != nil {
		return err
	}

	return nil
}

////////////

// splitKey splits `key` into it's parent container and base key
func (cfg *Config) splitKey(key string) (map[interface{}]interface{}, string) {
	return splitKeyRecursive(strings.Split(key, "."), cfg.memory)
}

// actual worker for splitKey
func splitKeyRecursive(keys []string, root map[interface{}]interface{}) (map[interface{}]interface{}, string) {
	if len(keys) == 0 {
		return nil, ""
	}

	child, ok := root[keys[0]]
	if !ok {
		return nil, ""
	}

	section, ok := child.(map[interface{}]interface{})
	if !ok {
		if len(keys) > 1 {
			return nil, ""
		}

		// scalar type, return immediately.
		return root, keys[0]
	}

	return splitKeyRecursive(keys[1:], section)
}

// get is the worker for the higher level typed accessors
func (cfg *Config) get(key string) interface{} {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	key = prefixKey(cfg.section, key)
	parent, base := cfg.splitKey(key)
	if parent == nil {
		panic(fmt.Sprintf("bug: invalid config key: %v", key))
	}

	return parent[base]
}

// set is worker behind the Set*() methods.
func (cfg *Config) set(key string, val interface{}) error {
	cfg.mu.Lock()

	key = prefixKey(cfg.section, key)
	callbacks := []callback{}
	defer func() {
		// Call the callbacks without the lock:
		for _, callback := range callbacks {
			callback.fn(callback.key)
		}
	}()

	// NOTE: the unlock is called before the other defer!
	defer cfg.mu.Unlock()

	parent, base := cfg.splitKey(key)
	if parent == nil {
		panic(fmt.Sprintf("bug: invalid config key: %v", key))
	}

	defType := getTypeOf(parent[base])
	valType := getTypeOf(val)

	if !isCompatibleType(defType, valType) {
		panic(
			fmt.Sprintf(
				"bug: wrong type in set for key `%v`: want: %v but got %v",
				key, defType, valType,
			),
		)
	}

	if parent[base] == val {
		// Nothing changed. No need to execute the callbacks.
		return nil
	}

	// If there is an validator defined, we should check now.
	defEntry := getDefaultByKey(key, cfg.defaults)
	if defEntry.Validator != nil {
		if err := defEntry.Validator(val); err != nil {
			return err
		}
	}

	parent[base] = val

	// Gather callbacks while still holding the lock:
	for _, ckey := range []string{key, ""} {
		if ckey == "" || strings.HasPrefix(ckey, cfg.section) {
			if bucket, ok := cfg.changeCallbacks[ckey]; ok {
				for _, callback := range bucket {
					callbacks = append(callbacks, callback)
				}
			}
		}
	}

	return nil
}

////////////

// AddChangedKeyEvent registers a callback to be called when `key` is changed.
// Special case: if key is the empy string, the registered callback will get
// called for every change (with the respective key)
// This function supports registering several callbacks for the same `key`.
// The returned id can be used to unregister a callback with RemoveChangedKeyEvent()
// Note: This function will panic when using an invalid key.
func (cfg *Config) AddChangedKeyEvent(key string, fn func(key string)) int {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	event := callback{
		fn:  fn,
		key: key,
	}

	if key != "" {
		key = prefixKey(cfg.section, key)
		defaultEntry := getDefaultByKey(key, cfg.defaults)
		if defaultEntry == nil {
			panic(fmt.Sprintf("bug: invalid config key: %v", key))
		}
	}

	callbacks, ok := cfg.changeCallbacks[key]
	if !ok {
		callbacks = make(map[int]callback)
		cfg.changeCallbacks[key] = callbacks
	}

	oldCount := cfg.callbackCount
	callbacks[oldCount] = event

	cfg.callbackCount++

	return oldCount
}

// RemoveChangedKeyEvent removes a previously registered callback.
// Note: This function will panic when using an invalid key.
func (cfg *Config) RemoveChangedKeyEvent(id int) error {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	toDelete := []string{}
	for key, bucket := range cfg.changeCallbacks {
		delete(bucket, id)
		if len(bucket) == 0 {
			toDelete = append(toDelete, key)
		}
	}

	for _, key := range toDelete {
		delete(cfg.changeCallbacks, key)
	}

	return nil
}

////////////

// Get returns the raw value at `key`.
// Do not use this method when possible, use the typeed convinience methods.
// Note: This function will panic if the key does not exist.
func (cfg *Config) Get(key string) interface{} {
	return cfg.get(key)
}

// Bool returns the boolean value (or default) at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) Bool(key string) bool {
	return cfg.get(key).(bool)
}

// String returns the string value (or default) at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) String(key string) string {
	return cfg.get(key).(string)
}

// Int returns the int value (or default) at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) Int(key string) int64 {
	return cfg.get(key).(int64)
}

// Float returns the float value (or default) at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) Float(key string) float64 {
	return cfg.get(key).(float64)
}

////////////

// SetBool creates or sets the `val` at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) SetBool(key string, val bool) error {
	return cfg.set(key, val)
}

// SetString creates or sets the `val` at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) SetString(key string, val string) error {
	return cfg.set(key, val)
}

// SetInt creates or sets the `val` at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) SetInt(key string, val int64) error {
	return cfg.set(key, val)
}

// SetFloat creates or sets the `val` at `key`.
// Note: This function will panic if the key does not exist.
func (cfg *Config) SetFloat(key string, val float64) error {
	return cfg.set(key, val)
}

// Set creates or sets the `val` at `key`.
// Please only use this function only if you have an interface{}
// that you do not want to cast yourself.
// Note: This function will panic if the key does not exist.
func (cfg *Config) Set(key string, val interface{}) error {
	return cfg.set(key, val)
}

////////////

// GetDefault retrieves the default for a certain key.
// Note: This function will panic if the key does not exist.
func (cfg *Config) GetDefault(key string) DefaultEntry {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	// The lock here is probably not necessary,
	// since we wont't modify defaults.
	key = prefixKey(cfg.section, key)
	entry := getDefaultByKey(key, cfg.defaults)
	if entry == nil {
		panic(fmt.Sprintf("bug: invalid config key: %v", key))
	}

	return *entry
}

// Keys returns all keys that are currently set (including the default keys)
func (cfg *Config) Keys() []string {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	allKeys := []string{}
	err := keys(cfg.memory, nil, func(section map[interface{}]interface{}, key []string) error {
		fullKey := strings.Join(key, ".")
		if strings.HasPrefix(fullKey, cfg.section) {
			allKeys = append(allKeys, strings.Join(key, "."))
		}

		return nil
	})

	if err != nil {
		// keys() should only return an error if the function passed to it
		// error in some way. Since we don't do that it should not produce
		// any non-nil error return.
		panic(fmt.Sprintf("Keys() failed internally: %v", err))
	}

	sort.Strings(allKeys)
	return allKeys
}

func (cfg *Config) Section(section string) *Config {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	childCallbackCount := cfg.callbackCount
	childChangeCallbacks := make(map[string]map[int]callback)

	for key, bucket := range cfg.changeCallbacks {
		childBucket := make(map[int]callback)
		childChangeCallbacks[key] = childBucket
		for _, callback := range bucket {
			childBucket[childCallbackCount] = callback
			childCallbackCount++
		}
	}

	return &Config{
		// mutex is shared with parent, since they protect the same memory.
		mu:            cfg.mu,
		section:       section,
		callbackCount: childCallbackCount,
		// The data is shared, any set to a section will cause a set in the parent.
		defaults: cfg.defaults,
		memory:   cfg.memory,
		// Sections may have own callbacks.
		// The parent callbacks are still called though.
		changeCallbacks: childChangeCallbacks,
	}
}

// IsValidKey can be checked to see if untrusted keys actually are valid.
// It should not be used to check keys from string literals.
func (cfg *Config) IsValidKey(key string) bool {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	key = prefixKey(cfg.section, key)
	return getDefaultByKey(key, cfg.defaults) != nil
}

// Cast takes `val` and reads the type of `key`.
// It then tries to convert it to one of the supported types
// (and possibly fails due to that)
//
// This cast assumes that `val` is always a string,
// which is useful for data coming fom the client.
// Note: This function will panic if the key does not exist.
func (cfg *Config) Cast(key, val string) (interface{}, error) {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	key = prefixKey(cfg.section, key)
	entry := getDefaultByKey(key, cfg.defaults)
	if entry == nil {
		panic(fmt.Sprintf("bug: invalid config key: %v", key))
	}

	switch entry.Default.(type) {
	case int, int16, int32, int64, uint, uint16, uint32, uint64:
		return strconv.ParseInt(val, 10, 64)
	case float32, float64:
		return strconv.ParseFloat(val, 64)
	case bool:
		return strconv.ParseBool(val)
	case string:
		return val, nil
	}

	return nil, nil
}

// FromFile creates a new config from the YAML file located at `path`
func FromFile(path string, defaults DefaultMapping) (*Config, error) {
	fd, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer fd.Close()
	return Open(fd, defaults)
}

// ToFile saves `cfg` as YAML at a file located at `path`.
func ToFile(path string, cfg *Config) error {
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	defer fd.Close()
	return cfg.Save(fd)
}
