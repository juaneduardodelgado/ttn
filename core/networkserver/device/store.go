// Copyright © 2017 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package device

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/TheThingsNetwork/ttn/core/storage"
	"github.com/TheThingsNetwork/ttn/core/types"
	"github.com/TheThingsNetwork/ttn/utils/errors"
	"gopkg.in/redis.v5"
)

// Store interface for Devices
type Store interface {
	List() ([]*Device, error)
	ListForAddress(devAddr types.DevAddr) ([]*Device, error)
	Get(appEUI types.AppEUI, devEUI types.DevEUI) (*Device, error)
	Set(new *Device, properties ...string) (err error)
	Activate(appEUI types.AppEUI, devEUI types.DevEUI, devAddr types.DevAddr, nwkSKey types.NwkSKey) error
	Delete(appEUI types.AppEUI, devEUI types.DevEUI) error

	PushFrame(appEUI types.AppEUI, devEUI types.DevEUI, frame *Frame) error
	GetFrames(appEUI types.AppEUI, devEUI types.DevEUI) ([]*Frame, error)
	ClearFrames(appEUI types.AppEUI, devEUI types.DevEUI) error
}

const defaultRedisPrefix = "ns"

const redisDevicePrefix = "device"
const redisDevAddrPrefix = "dev_addr"
const redisFramesPrefix = "frames"

// FramesHistorySize for ADR
const FramesHistorySize = 20

// NewRedisDeviceStore creates a new Redis-based status store
func NewRedisDeviceStore(client *redis.Client, prefix string) Store {
	if prefix == "" {
		prefix = defaultRedisPrefix
	}
	store := storage.NewRedisMapStore(client, prefix+":"+redisDevicePrefix)
	store.SetBase(Device{}, "")

	return &RedisDeviceStore{
		client:       client,
		prefix:       prefix,
		store:        store,
		devAddrIndex: storage.NewRedisSetStore(client, prefix+":"+redisDevAddrPrefix),
	}
}

// RedisDeviceStore stores Devices in Redis.
// - Devices are stored as a Hash
// - DevAddr mappings are indexed in a Set
type RedisDeviceStore struct {
	client       *redis.Client
	prefix       string
	store        *storage.RedisMapStore
	devAddrIndex *storage.RedisSetStore
}

// List all Devices
func (s *RedisDeviceStore) List() ([]*Device, error) {
	devicesI, err := s.store.List("", nil)
	if err != nil {
		return nil, err
	}
	devices := make([]*Device, 0, len(devicesI))
	for _, deviceI := range devicesI {
		if device, ok := deviceI.(Device); ok {
			devices = append(devices, &device)
		}
	}
	return devices, nil
}

// ListForAddress lists all devices for a specific DevAddr
func (s *RedisDeviceStore) ListForAddress(devAddr types.DevAddr) ([]*Device, error) {
	deviceKeys, err := s.devAddrIndex.Get(devAddr.String())
	if errors.GetErrType(err) == errors.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	devicesI, err := s.store.GetAll(deviceKeys, nil)
	if err != nil {
		return nil, err
	}
	devices := make([]*Device, 0, len(devicesI))
	for _, deviceI := range devicesI {
		if device, ok := deviceI.(Device); ok {
			devices = append(devices, &device)
		}
	}
	return devices, nil
}

// Get a specific Device
func (s *RedisDeviceStore) Get(appEUI types.AppEUI, devEUI types.DevEUI) (*Device, error) {
	deviceI, err := s.store.Get(fmt.Sprintf("%s:%s", appEUI, devEUI))
	if err != nil {
		return nil, err
	}
	if device, ok := deviceI.(Device); ok {
		return &device, nil
	}
	return nil, errors.New("Database did not return a Device")
}

// Set a new Device or update an existing one
func (s *RedisDeviceStore) Set(new *Device, properties ...string) (err error) {
	// If this is an update, check if AppEUI, DevEUI and DevAddr are still the same
	old := new.old
	var addrChanged bool
	if old != nil {
		addrChanged = new.DevAddr != old.DevAddr || new.DevEUI != old.DevEUI || new.AppEUI != old.AppEUI
		if addrChanged {
			if err := s.devAddrIndex.Remove(old.DevAddr.String(), fmt.Sprintf("%s:%s", old.AppEUI, old.DevEUI)); err != nil {
				return err
			}
		}
	}

	now := time.Now()
	new.UpdatedAt = now

	key := fmt.Sprintf("%s:%s", new.AppEUI, new.DevEUI)
	if new.old != nil {
		err = s.store.Update(key, *new, properties...)
	} else {
		new.CreatedAt = now
		err = s.store.Create(key, *new, properties...)
	}
	if err != nil {
		return
	}

	if (new.old == nil || addrChanged) && !new.DevAddr.IsEmpty() {
		if err := s.devAddrIndex.Add(new.DevAddr.String(), key); err != nil {
			return err
		}
	}

	return nil
}

// Activate a Device
func (s *RedisDeviceStore) Activate(appEUI types.AppEUI, devEUI types.DevEUI, devAddr types.DevAddr, nwkSKey types.NwkSKey) error {
	dev, err := s.Get(appEUI, devEUI)
	if err != nil {
		return err
	}

	dev.StartUpdate()

	dev.LastSeen = time.Now()
	dev.UpdatedAt = time.Now()
	dev.DevAddr = devAddr
	dev.NwkSKey = nwkSKey
	dev.FCntUp = 0
	dev.FCntDown = 0

	return s.Set(dev)
}

// Delete a Device
func (s *RedisDeviceStore) Delete(appEUI types.AppEUI, devEUI types.DevEUI) error {
	key := fmt.Sprintf("%s:%s", appEUI, devEUI)

	deviceI, err := s.store.GetFields(key, "dev_addr")
	if err != nil {
		return err
	}

	device, ok := deviceI.(Device)
	if !ok {
		errors.New("Database did not return a Device")
	}

	if !device.DevAddr.IsEmpty() {
		if err := s.devAddrIndex.Remove(device.DevAddr.String(), key); err != nil {
			return err
		}
	}

	return s.store.Delete(key)
}

func (s *RedisDeviceStore) framesKey(appEUI types.AppEUI, devEUI types.DevEUI) string {
	return fmt.Sprintf("%s:%s:%s:%s", s.prefix, redisFramesPrefix, appEUI, devEUI)
}

// PushFrame pushes a Frame to the device's history
func (s *RedisDeviceStore) PushFrame(appEUI types.AppEUI, devEUI types.DevEUI, frame *Frame) error {
	frameBytes, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	key := s.framesKey(appEUI, devEUI)
	pipe := s.client.Pipeline()
	defer pipe.Close()
	pipe.LPush(key, frameBytes)
	pipe.LTrim(key, 0, FramesHistorySize-1)
	_, err = pipe.Exec()
	if err != nil {
		return err
	}
	return nil
}

// GetFrames retrieves the last frames from the device's history
func (s *RedisDeviceStore) GetFrames(appEUI types.AppEUI, devEUI types.DevEUI) (out []*Frame, err error) {
	frames, err := s.client.LRange(s.framesKey(appEUI, devEUI), 0, -1).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	for _, frameStr := range frames {
		frame := new(Frame)
		if err := json.Unmarshal([]byte(frameStr), frame); err != nil {
			return nil, err
		}
		out = append(out, frame)
	}
	return
}

// ClearFrames clears the frames in the device's history
func (s *RedisDeviceStore) ClearFrames(appEUI types.AppEUI, devEUI types.DevEUI) error {
	return s.client.Del(s.framesKey(appEUI, devEUI)).Err()
}
