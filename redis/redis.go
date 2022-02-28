package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const (
	SyncChannelName  = "blocky_sync"
	CacheStorePrefix = "blocky:cache:"
	chanCap          = 1000
	cacheReason      = "EXTERNAL_CACHE"
	defaultCacheTime = 1 * time.Second
)

// messageType enum
type messageType uint

const (
	messageTypeCache = iota
	messageTypeEnable
)

func (t *messageType) String() string {
	switch *t {
	case messageTypeCache:
		return "cache"
	case messageTypeEnable:
		return "enable"
	default:
		return "invalid"
	}
}

// sendBuffer message
type bufferMessage struct {
	Key     string
	Message *dns.Msg
}

// redis pubsub message
type redisMessage struct {
	Key     string      `json:"k,omitempty"`
	Type    messageType `json:"t"`
	Message []byte      `json:"m"`
	Client  []byte      `json:"c"`
}

// CacheChannel message
type CacheMessage struct {
	Key      string
	Response *model.Response
}

type EnabledMessage struct {
	State    bool          `json:"s"`
	Duration time.Duration `json:"d,omitempty"`
	Groups   []string      `json:"g,omitempty"`
}

// Client for redis communication
type Client struct {
	config         *config.RedisConfig
	client         *redis.Client
	l              *logrus.Entry
	ctx            context.Context
	cancel         context.CancelFunc
	id             []byte
	sendBuffer     chan *bufferMessage
	CacheChannel   chan *CacheMessage
	EnabledChannel chan *EnabledMessage
}

// New creates a new redis client
func New(cfg *config.RedisConfig) (*Client, error) {
	// disable redis if no address is provided
	if cfg == nil || !cfg.Enable {
		return nil, nil
	}

	if len(cfg.Address) == 0 {
		return nil, errors.New("redis is enabled but has no address")
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:            cfg.Address,
		Password:        cfg.Password,
		DB:              cfg.Database,
		MaxRetries:      cfg.ConnectionAttempts,
		MaxRetryBackoff: time.Duration(cfg.ConnectionCooldown),
	})

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()

	_, err := rdb.Ping(pingCtx).Result()
	if err != nil {
		return nil, err
	}

	id, err := uuid.New().MarshalBinary()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	// construct client
	res := &Client{
		config:         cfg,
		client:         rdb,
		l:              log.PrefixedLog("redis"),
		ctx:            ctx,
		cancel:         cancel,
		id:             id,
		sendBuffer:     make(chan *bufferMessage, chanCap),
		CacheChannel:   make(chan *CacheMessage, chanCap),
		EnabledChannel: make(chan *EnabledMessage, chanCap),
	}

	// start channel handling go routine
	err = res.startup()

	return res, err
}

func (c *Client) Close() error {
	c.cancel()
	return c.client.Close()
}

// PublishCache publish cache to redis async
func (c *Client) PublishCache(key string, message *dns.Msg) {
	if len(key) > 0 && message != nil {
		c.sendBuffer <- &bufferMessage{
			Key:     key,
			Message: message,
		}
	}
}

func (c *Client) PublishEnabled(state *EnabledMessage) {
	binState, err := json.Marshal(state)
	if err != nil {
		c.l.Error("Could not marshal state to JSON: ", err)
		return
	}

	binMsg, err := json.Marshal(redisMessage{
		Type:    messageTypeEnable,
		Message: binState,
		Client:  c.id,
	})
	if err != nil {
		c.l.Error("Could not marshal Redis message to JSON: ", err)
		return
	}

	cmd := c.client.Publish(c.ctx, SyncChannelName, binMsg)
	if cmd.Err() != nil {
		c.l.Error("Could not publish state to Redis: ", err)
	}
}

// GetRedisCache reads the redis cache and publish it to the channel
func (c *Client) GetRedisCache() {
	c.l.Debug("GetRedisCache")

	go func() {
		iter := c.client.Scan(c.ctx, 0, prefixKey("*"), 0).Iterator()
		for iter.Next(c.ctx) {
			response, err := c.getResponse(iter.Val())
			if err != nil {
				c.l.Error("GetRedisCache ", err)
				continue
			}

			if response != nil {
				c.CacheChannel <- response
			}
		}
	}()
}

// startup starts a new goroutine for subscription and translation
func (c *Client) startup() error {
	ps := c.client.Subscribe(c.ctx, SyncChannelName)

	_, err := ps.Receive(c.ctx)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			// receive message from subscription
			case msg := <-ps.Channel():
				err := c.processReceivedMessage(msg)
				if err != nil {
					c.l.Error("Could not process received message: ", err)
				}

			// publish message from buffer
			case s := <-c.sendBuffer:
				err := c.publishMessageFromBuffer(s)
				if err != nil {
					c.l.Error("Could not publish message: ", err)
				}
			}
		}
	}()

	return nil
}

func (c *Client) publishMessageFromBuffer(s *bufferMessage) error {
	origRes := s.Message
	origRes.Compress = true

	binRes, err := origRes.Pack()
	if err != nil {
		return err
	}

	binMsg, err := json.Marshal(redisMessage{
		Key:     s.Key,
		Type:    messageTypeCache,
		Message: binRes,
		Client:  c.id,
	})
	if err != nil {
		return err
	}

	cmd := c.client.Publish(c.ctx, SyncChannelName, binMsg)
	if cmd.Err() != nil {
		return cmd.Err()
	}

	return c.client.Set(c.ctx, prefixKey(s.Key), binRes, c.getTTL(origRes)).Err()
}

func (c *Client) processReceivedMessage(msg *redis.Message) (err error) {
	c.l.Debug("Received message: ", msg)

	if msg == nil || len(msg.Payload) == 0 {
		return nil
	}

	var rm redisMessage

	err = json.Unmarshal([]byte(msg.Payload), &rm)
	if err != nil {
		return err
	}

	if bytes.Equal(rm.Client, c.id) {
		// message sent from this blocky instance, ignore
		return nil
	}

	switch rm.Type {
	case messageTypeCache:
		cm, err := convertMessage(&rm, 0)
		if err != nil {
			return err
		}
		c.CacheChannel <- cm
	case messageTypeEnable:
		err := c.processEnableMessage(&rm)
		if err != nil {
			return fmt.Errorf("error processing enable message: %w", err)
		}
	default:
		return fmt.Errorf("unknown message type: %s", rm.Type.String())
	}

	return nil
}

func (c *Client) processEnableMessage(redisMsg *redisMessage) error {
	var msg EnabledMessage

	err := json.Unmarshal(redisMsg.Message, &msg)
	if err != nil {
		return err
	}

	c.EnabledChannel <- &msg

	return nil
}

// getResponse returns model.Response for a key
func (c *Client) getResponse(key string) (*CacheMessage, error) {
	resp, err := c.client.Get(c.ctx, key).Result()
	if err != nil {
		return nil, err
	}

	ttl, err := c.client.TTL(c.ctx, key).Result()
	if err != nil {
		return nil, err
	}

	result, err := convertMessage(&redisMessage{
		Key:     cleanKey(key),
		Message: []byte(resp),
	}, ttl)
	if err != nil {
		return nil, fmt.Errorf("conversion error: %w", err)
	}

	return result, nil
}

// convertMessage converts redisMessage to CacheMessage
func convertMessage(message *redisMessage, ttl time.Duration) (*CacheMessage, error) {
	var msg dns.Msg

	err := msg.Unpack(message.Message)
	if err != nil {
		return nil, err
	}

	if ttl > 0 {
		for _, a := range msg.Answer {
			a.Header().Ttl = uint32(ttl.Seconds())
		}
	}

	res := &CacheMessage{
		Key: message.Key,
		Response: &model.Response{
			RType:  model.ResponseTypeCACHED,
			Reason: cacheReason,
			Res:    &msg,
		},
	}

	return res, nil
}

// getTTL of dns message or return defaultCacheTime if 0
func (c *Client) getTTL(dns *dns.Msg) time.Duration {
	ttl := uint32(0)
	for _, a := range dns.Answer {
		if a.Header().Ttl > ttl {
			ttl = a.Header().Ttl
		}
	}

	if ttl == 0 {
		return defaultCacheTime
	}

	return time.Duration(ttl) * time.Second
}

// prefixKey with CacheStorePrefix
func prefixKey(key string) string {
	return CacheStorePrefix + key
}

// cleanKey trims CacheStorePrefix prefix
func cleanKey(key string) string {
	return strings.TrimPrefix(key, CacheStorePrefix)
}
