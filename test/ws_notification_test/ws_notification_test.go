/*
 * Copyright 2018 The Service Manager Authors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package ws_notification_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/Peripli/service-manager/api/notifications"

	"github.com/spf13/pflag"

	"github.com/Peripli/service-manager/pkg/util"

	"github.com/Peripli/service-manager/pkg/types"

	"github.com/Peripli/service-manager/pkg/web"

	"github.com/Peripli/service-manager/storage"

	"github.com/gorilla/websocket"

	"github.com/Peripli/service-manager/pkg/env"
	"github.com/Peripli/service-manager/pkg/sm"
	"github.com/Peripli/service-manager/test/common"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestWsConn(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Notification test suite")
}

var pingTimeout time.Duration = 1 * time.Second

var _ = Describe("WS", func() {
	var ctx *common.TestContext
	var wsconn *websocket.Conn
	queryParams := map[string]string{}
	var resp *http.Response
	var repository storage.Repository
	var platform *types.Platform
	var connections []*websocket.Conn

	wsconnect := func(platform *types.Platform, queryParams map[string]string) (*websocket.Conn, *http.Response, error) {
		smURL := ctx.Servers[common.SMServer].URL()
		smEndpoint, _ := url.Parse(smURL)
		smEndpoint.Scheme = "ws"
		smEndpoint.Path = web.NotificationsURL
		q := smEndpoint.Query()
		for k, v := range queryParams {
			q.Add(k, v)
		}
		smEndpoint.RawQuery = q.Encode()

		headers := http.Header{}
		encodedPlatform := base64.StdEncoding.EncodeToString([]byte(platform.Credentials.Basic.Username + ":" + platform.Credentials.Basic.Password))
		headers.Add("Authorization", "Basic "+encodedPlatform)

		wsEndpoint := smEndpoint.String()
		conn, resp, err := websocket.DefaultDialer.Dial(wsEndpoint, headers)
		if conn != nil {
			connections = append(connections, conn)
		}
		return conn, resp, err
	}

	wsconnectWithPlatform := func() (*types.Platform, *websocket.Conn, *http.Response, error) {
		platform := common.RegisterPlatformInSM(common.GenerateRandomPlatform(), ctx.SMWithOAuth, map[string]string{})
		conn, resp, err := wsconnect(platform, nil)
		return platform, conn, resp, err
	}

	BeforeEach(func() {
		queryParams = map[string]string{}

		ctx = common.NewTestContextBuilder().
			WithEnvPreExtensions(func(set *pflag.FlagSet) {
				set.Set("websocket.ping_timeout", pingTimeout.String())
			}).
			WithSMExtensions(func(ctx context.Context, smb *sm.ServiceManagerBuilder, e env.Environment) error {
				repository = smb.Storage
				return nil
			}).Build()
		Expect(repository).ToNot(BeNil())

		platform = common.RegisterPlatformInSM(common.GenerateRandomPlatform(), ctx.SMWithOAuth, map[string]string{})
	})

	JustBeforeEach(func() {
		var err error
		wsconn, resp, err = wsconnect(platform, queryParams)
		Expect(err).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		if repository != nil {
			_, err := repository.Delete(context.Background(), types.NotificationType)
			if err != nil {
				Expect(err).To(Equal(util.ErrNotFoundInStorage))
			}
		}
		ctx.Cleanup()
	})

	JustAfterEach(func() {
		for _, conn := range connections {
			conn.Close()
		}
		connections = nil
	})

	Context("when non-websocket request is received", func() {
		It("should reject it", func() {
			ctx.SMWithBasic.GET(web.NotificationsURL).Expect().
				Status(http.StatusBadRequest).
				JSON().Object().Value("error").Equal("WebsocketUpgradeError")
		})
	})

	Context("when ping is received", func() {
		var pongCh chan struct{}

		JustBeforeEach(func() {
			pongCh = make(chan struct{})
			wsconn.SetReadDeadline(time.Time{})
			wsconn.SetPongHandler(func(data string) error {
				Expect(data).To(Equal("pingping"))
				close(pongCh)
				return nil
			})
			go func() {
				_, _, err := wsconn.ReadMessage()
				Expect(err).Should(HaveOccurred())
			}()
		})

		It("should respond with pong", func(done Done) {
			err := wsconn.WriteMessage(websocket.PingMessage, []byte("pingping"))
			Expect(err).ShouldNot(HaveOccurred())
			Eventually(pongCh).Should(BeClosed())
			close(done)
		})
	})

	Context("when ping is not sent on time", func() {
		It("should close the connection", func(done Done) {
			wsconn.SetCloseHandler(func(code int, msg string) error {
				close(done)
				return nil
			})
			go func() {
				wsconn.ReadMessage()
			}()
		}, pingTimeout.Seconds()+1)
	})

	Context("when no notifications are present", func() {
		It("should receive last known revision response header 0", func() {
			Expect(resp.Header.Get(notifications.LastKnownRevisionHeader)).To(Equal("0"))
		})

		It("should receive max ping timeout response header", func() {
			Expect(resp.Header.Get(notifications.MaxPingPeriodHeader)).ToNot(BeEmpty())
		})

		Context("and revision known to proxy is 0", func() {
			It("should receive 410 Gone", func() {
				queryParams[notifications.LastKnownRevisionQueryParam] = "0"
				_, resp, _ := wsconnect(platform, queryParams)
				Expect(resp.StatusCode).To(Equal(http.StatusGone))
			})
		})
	})

	Context("when notifications are created prior to connection", func() {
		var notification *types.Notification
		var notificationRevision int64
		BeforeEach(func() {
			notification, notificationRevision = createNotification(repository, platform.ID)
		})

		It("should receive last known revision response header greater than 0", func() {
			lastKnownRevision, err := strconv.Atoi(resp.Header.Get(notifications.LastKnownRevisionHeader))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(lastKnownRevision).To(BeNumerically(">", 0))
		})

		It("should receive them", func() {
			expectNotification(wsconn, notification.ID, notification.PlatformID)
		})

		Context("and revision known to proxy is 0", func() {
			It("should receive 410 Gone", func() {
				queryParams[notifications.LastKnownRevisionQueryParam] = "0"
				_, resp, _ := wsconnect(platform, queryParams)
				Expect(resp.StatusCode).To(Equal(http.StatusGone))
			})
		})

		Context("and proxy knowns some notification revision", func() {
			var notification2 *types.Notification
			BeforeEach(func() {
				notification2, _ = createNotification(repository, platform.ID)
				queryParams[notifications.LastKnownRevisionQueryParam] = strconv.FormatInt(notificationRevision, 10)
			})

			It("should receive only these after the revision that it knowns", func() {
				expectNotification(wsconn, notification2.ID, notification2.PlatformID)
			})
		})

		Context("and revision known to proxy is not known to sm anymore", func() {
			It("should receive 410 Gone", func() {
				queryParams[notifications.LastKnownRevisionQueryParam] = strconv.FormatInt(notificationRevision-1, 10)
				_, resp, err := wsconnect(platform, queryParams)
				Expect(resp.StatusCode).To(Equal(http.StatusGone))
				Expect(err).Should(HaveOccurred())
			})
		})

		Context("and proxy known revision is greater than sm known revision", func() {
			It("should receive 410 Gone", func() {
				queryParams[notifications.LastKnownRevisionQueryParam] = strconv.FormatInt(notificationRevision+1, 10)
				_, resp, err := wsconnect(platform, queryParams)
				Expect(resp.StatusCode).To(Equal(http.StatusGone))
				Expect(err).Should(HaveOccurred())
			})
		})

		Context("when multiple connections are opened", func() {
			It("all other should not receive prior notifications, but only newly created", func() {
				wsconns := make([]*websocket.Conn, 0)
				createdNotifications := make([]*types.Notification, 0)
				for i := 0; i < 5; i++ {
					pl, conn, _, err := wsconnectWithPlatform()

					Expect(err).ShouldNot(HaveOccurred())
					wsconns = append(wsconns, conn)

					n, _ := createNotification(repository, pl.ID)
					createdNotifications = append(createdNotifications, n)
				}

				for i, conn := range wsconns {
					expectNotification(conn, createdNotifications[i].ID, createdNotifications[i].PlatformID)
				}
			})
		})
	})

	Context("when same platform is connected twice", func() {
		It("should send same notifications to both", func() {
			conn, _, err := wsconnect(platform, queryParams)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(conn).ShouldNot(BeNil())

			notification, _ := createNotification(repository, platform.ID)
			expectNotification(conn, notification.ID, platform.ID)
			expectNotification(wsconn, notification.ID, platform.ID)
		})
	})

	Context("when revision known to proxy is invalid number", func() {
		It("should return status 400", func() {
			queryParams[notifications.LastKnownRevisionQueryParam] = "not_a_number"
			_, resp, err := wsconnect(platform, queryParams)
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(err).Should(HaveOccurred())
		})
	})

	Context("when notification are created after ws conn is created", func() {
		It("should receive new notifications", func() {
			notification, _ := createNotification(repository, platform.ID)
			expectNotification(wsconn, notification.ID, notification.PlatformID)
		})
	})

	Context("when one notification with empty platform and one notification with platform are created", func() {
		var notificationEmptyPlatform, notification *types.Notification
		BeforeEach(func() {
			notification, _ = createNotification(repository, platform.ID)
			notificationEmptyPlatform, _ = createNotification(repository, "")
		})

		It("one connection should receive both, but other only the one with empty platform", func() {
			expectNotification(wsconn, notification.ID, notification.PlatformID)
			expectNotification(wsconn, notificationEmptyPlatform.ID, "")

			By("creating new connection")
			_, newWsConn, _, err := wsconnectWithPlatform()
			Expect(err).ShouldNot(HaveOccurred())
			expectNotification(newWsConn, notificationEmptyPlatform.ID, "")
		})
	})

})

func createNotification(repository storage.Repository, platformID string) (*types.Notification, int64) {
	notification := common.GenerateRandomNotification()
	notification.PlatformID = platformID
	id, err := repository.Create(context.Background(), notification)
	Expect(err).ShouldNot(HaveOccurred())

	createdNotification, err := repository.Get(context.Background(), types.NotificationType, id)
	Expect(err).ShouldNot(HaveOccurred())
	notificationRevision := (createdNotification.(*types.Notification)).Revision

	return notification, notificationRevision
}

func expectNotification(wsconn *websocket.Conn, notificationID, platformID string) {
	notification := readNotification(wsconn)
	Expect(notification["id"]).To(Equal(notificationID))
	if platformID == "" {
		Expect(notification["platform_id"]).To(BeNil())
	} else {
		Expect(notification["platform_id"]).To(Equal(platformID))
	}
}

func readNotification(wsconn *websocket.Conn) map[string]interface{} {
	var r map[string]interface{}
	err := wsconn.ReadJSON(&r)
	Expect(err).ShouldNot(HaveOccurred())
	return r
}
