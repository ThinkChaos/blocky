package redis

import (
	"encoding/json"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/util"
	"github.com/alicebob/miniredis/v2"
	"github.com/creasty/defaults"
	"github.com/google/uuid"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Redis client", func() {
	var (
		redisServer *miniredis.Miniredis
		redisClient *Client
		redisConfig *config.RedisConfig
		err         error
	)

	BeforeEach(func() {
		redisServer, err = miniredis.Run()
		Expect(err).Should(Succeed())

		redisConfig = &config.RedisConfig{}
		err = defaults.Set(redisConfig)
		Expect(err).Should(Succeed())

		redisConfig.Enable = true
		redisConfig.Address = redisServer.Addr()
	})

	JustBeforeEach(func() {
		redisClient, err = New(redisConfig)
		Expect(err).Should(Succeed())
		Expect(redisClient).ShouldNot(BeNil())
	})

	AfterEach(func() {
		redisClient.Close()
		redisServer.Close()
	})

	Describe("Creation", func() {
		When("disabled", func() {
			It("should be nil", func() {
				redisConfig.Enable = false

				client, err := New(redisConfig)
				Expect(err).Should(Succeed())
				Expect(client).Should(BeNil())
			})
		})
		When("has no address", func() {
			It("should error", func() {
				redisConfig.Address = ""

				client, err := New(redisConfig)
				Expect(err).ShouldNot(Succeed())
				Expect(client).Should(BeNil())
			})
		})
		When("has invalid address", func() {
			It("should error", func() {
				redisConfig.Address = "test:123"

				_, err = New(redisConfig)
				Expect(err).ShouldNot(Succeed())
			})
		})
		When("has invalid password", func() {
			It("should error", func() {
				redisConfig.Password = "wrong"

				_, err = New(redisConfig)
				Expect(err).ShouldNot(Succeed())
			})
		})
	})

	When("publish", func() {
		It("cache works", func() {
			res, err := util.NewMsgWithAnswer("example.com.", 123, dns.TypeA, "123.124.122.123")
			Expect(err).Should(Succeed())
			Expect(res).ShouldNot(BeNil())

			redisClient.PublishCache("example.com", res)
			Eventually(func() []string {
				return redisServer.DB(redisConfig.Database).Keys()
			}, "100ms").Should(HaveLen(1))
		})
		It("enabled works", func() {
			redisClient.PublishEnabled(&EnabledMessage{
				State: true,
			})
			Eventually(func() map[string]int {
				return redisServer.PubSubNumSub(SyncChannelName)
			}, "50ms").Should(HaveLen(1))
		})
	})

	Describe("Receiving", func() {
		var msg redisMessage

		JustBeforeEach(func() {
			id, err := uuid.New().MarshalBinary()
			Expect(err).Should(Succeed())

			msg.Client = id

			binMsg, err := json.Marshal(msg)
			Expect(err).Should(Succeed())

			rec := redisServer.Publish(SyncChannelName, string(binMsg))
			Expect(rec).Should(Equal(1))
		})

		When("receives enable message", func() {
			BeforeEach(func() {
				binState, err := json.Marshal(EnabledMessage{State: true})
				Expect(err).Should(Succeed())

				msg = redisMessage{
					Type:    messageTypeEnable,
					Message: binState,
				}
			})
			It("enables", func() {
				Eventually(func() chan *EnabledMessage {
					return redisClient.EnabledChannel
				}, "100ms").Should(HaveLen(1))
			})
		})
		When("receives invalid message", func() {
			BeforeEach(func() {
				msg = redisMessage{
					Key:     "unknown",
					Type:    99,
					Message: []byte("test"),
				}
			})
			It("ignores it", func() {
				Eventually(func() chan *EnabledMessage {
					return redisClient.EnabledChannel
				}, "100ms").Should(HaveLen(0))

				Eventually(func() chan *CacheMessage {
					return redisClient.CacheChannel
				}, "100ms").Should(HaveLen(0))
			})
		})
	})
	When("GetRedisCache", func() {
		It("works", func() {
			origCount := len(redisClient.CacheChannel)

			res, err := util.NewMsgWithAnswer("example.com.", 123, dns.TypeA, "123.124.122.123")
			Expect(err).Should(Succeed())

			redisClient.PublishCache("example.com", res)

			Eventually(func() []string {
				return redisServer.DB(redisConfig.Database).Keys()
			}, "100ms").Should(HaveLen(1))

			redisClient.GetRedisCache()

			Eventually(func() []string {
				return redisServer.DB(redisConfig.Database).Keys()
			}, "100ms").Should(HaveLen(origCount + 1))

		})
	})
})
