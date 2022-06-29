package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/coderjw77/rdpgo/guac"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

type ReqArg struct {
	GuacadAddr    string `form:"guacad_addr"`
	AssetProtocol string `form:"asset_protocol"`
	AssetHost     string `form:"asset_host"`
	AssetPort     string `form:"asset_port"`
	AssetUser     string `form:"asset_user"`
	AssetPassword string `form:"asset_password"`
	ScreenWidth   int    `form:"screen_width"`
	ScreenHeight  int    `form:"screen_height"`
	ScreenDpi     int    `form:"screen_dpi"`
}

//ApiWsGuacamole websocket 转 guacamole协议
func ApiWsGuacamole() gin.HandlerFunc {
	//0. 初始化 websocket 配置
	websocketReadBufferSize := guac.MaxGuacMessage
	websocketWriteBufferSize := guac.MaxGuacMessage * 2
	upgrade := websocket.Upgrader{
		ReadBufferSize:  websocketReadBufferSize,
		WriteBufferSize: websocketWriteBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			//检查origin 限定websocket 被其他的域名访问
			return true
		},
	}
	return func(c *gin.Context) {
		arg := new(ReqArg)
		err := c.BindQuery(arg)
		if err != nil {
			c.JSON(202, err.Error())
			return
		}
		protocol := c.Request.Header.Get("Sec-Websocket-Protocol")
		ws, err := upgrade.Upgrade(c.Writer, c.Request, http.Header{
			"Sec-Websocket-Protocol": {protocol},
		})
		if err != nil {
			logrus.WithError(err).Error("升级ws失败")
			return
		}
		defer func() {
			if err = ws.Close(); err != nil {
				logrus.Traceln("Error closing websocket", err)
			}
		}()

		uid := ""
		pipeTunnel, err := guac.NewGuacamoleTunnel(arg.GuacadAddr, arg.AssetProtocol, arg.AssetHost, arg.AssetPort, arg.AssetUser, arg.AssetPassword, uid, arg.ScreenWidth, arg.ScreenHeight, arg.ScreenDpi)
		if err != nil {
			logrus.Error("Failed to upgrade websocket", err)
			return
		}
		defer func() {
			if err = pipeTunnel.Close(); err != nil {
				logrus.Traceln("Error closing pipeTunnel", err)
			}
		}()

		ioCopy(ws, pipeTunnel)
		logrus.Info("websocket session end")
	}
}

func ioCopy(ws *websocket.Conn, tunnl *guac.SimpleTunnel) {
	writer := tunnl.AcquireWriter()
	reader := tunnl.AcquireReader()
	//if pipeTunnel.OnDisconnectWs != nil {
	//	defer pipeTunnel.OnDisconnectWs(id, ws, c.Request, pipeTunnel.TunnelPipe)
	//}
	defer tunnl.ReleaseWriter()
	defer tunnl.ReleaseReader()

	//使用 errgroup 来处理(管理) goroutine for-loop, 防止 for-goroutine zombie
	eg, _ := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		buf := bytes.NewBuffer(make([]byte, 0, guac.MaxGuacMessage*2))

		for {
			ins, err := reader.ReadSome()
			if err != nil {
				return err
			}

			if bytes.HasPrefix(ins, guac.InternalOpcodeIns) {
				// messages starting with the InternalDataOpcode are never sent to the websocket
				continue
			}

			if _, err = buf.Write(ins); err != nil {
				return err
			}

			// if the buffer has more data in it or we've reached the max buffer size, send the data and reset
			if !reader.Available() || buf.Len() >= guac.MaxGuacMessage {
				if err = ws.WriteMessage(1, buf.Bytes()); err != nil {
					if err == websocket.ErrCloseSent {
						return fmt.Errorf("websocket:%v", err)
					}
					logrus.Traceln("Failed sending message to ws", err)
					return err
				}
				buf.Reset()
			}
		}

	})
	eg.Go(func() error {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				logrus.Traceln("Error reading message from ws", err)
				return err
			}
			if bytes.HasPrefix(data, guac.InternalOpcodeIns) {
				// messages starting with the InternalDataOpcode are never sent to guacd
				continue
			}
			if _, err = writer.Write(data); err != nil {
				logrus.Traceln("Failed writing to guacd", err)
				return err
			}
		}

	})
	if err := eg.Wait(); err != nil {
		logrus.WithError(err).Error("session-err")
	}
}
