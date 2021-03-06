package program

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/etcd-manage/etcd-manage-server/program/cache"
	"github.com/etcd-manage/etcd-manage-server/program/logger"
	"github.com/etcd-manage/etcd-manage-server/program/models"
	"github.com/etcd-manage/etcdsdk"
	"github.com/etcd-manage/etcdsdk/model"
	"github.com/gin-gonic/autotls"
	gin "github.com/gin-gonic/gin"
)

// http服务

func (p *Program) startAPI() {
	router := gin.Default()

	// 跨域问题
	router.Use(p.middlewareCORS())
	router.Use(p.middlewareAuth())
	router.Use(p.middlewareEtcdClient())

	// 设置静态文件目录
	router.GET("/ui/*w", p.handlerStatic)
	router.GET("/", func(c *gin.Context) {
		c.Redirect(301, "/ui")
	})

	// 启动所有版本api
	for key, val := range p.vApis {
		vAPI := router.Group("/" + key)
		vAPI.Use()
		val.Register(vAPI)
	}

	addr := fmt.Sprintf("%s:%d", p.cfg.HTTP.Address, p.cfg.HTTP.Port)
	// 监听
	s := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	log.Println("Start HTTP the service:", addr)
	var err error
	if p.cfg.HTTP.TLSEnable == true {
		if p.cfg.HTTP.TLSConfig == nil || p.cfg.HTTP.TLSConfig.CertFile == "" || p.cfg.HTTP.TLSConfig.KeyFile == "" {
			log.Fatalln("Enable tls must configure certificate file path")
		}
		err = s.ListenAndServeTLS(p.cfg.HTTP.TLSConfig.CertFile, p.cfg.HTTP.TLSConfig.KeyFile)
	} else if p.cfg.HTTP.TLSEncryptEnable == true {
		if len(p.cfg.HTTP.TLSEncryptDomainNames) == 0 {
			log.Fatalln("The domain name list cannot be empty")
		}
		err = autotls.Run(router, p.cfg.HTTP.TLSEncryptDomainNames...)
	} else {
		err = s.ListenAndServe()
	}
	if err != nil {
		log.Fatalln(err)
	}

}

// middlewareAuth 获取是否登录
func (p *Program) middlewareAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, _ := url.ParseRequestURI(c.Request.RequestURI)
		if strings.HasPrefix(u.Path, "/v1/passport") == false && strings.HasPrefix(u.Path, "/ui") == false && strings.HasPrefix(u.Path, "/v1/upload") == false {
			// log.Println(u.Path)
			token := c.Request.Header.Get("Token")
			if token == "" {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			// 获取用户信息
			key := cache.GetLoginKey(token)
			val, exist := cache.DefaultMemCache.Get(key)
			if exist == false {
				logger.Log.Warnw("用户登录信息不存在")
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			// 解析为用户登录信息
			user := new(models.UsersModel)
			err := json.Unmarshal([]byte(val), user)
			if err != nil {
				logger.Log.Warnw("用户登录信息解析json错误", "err", err)
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			// 存储用户信息到上下文
			c.Set("userinfo", user)
		}
	}
}

// 跨域中间件
func (p *Program) middlewareCORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("origin")
		method := c.Request.Method

		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Headers", "Content-Type,AccessToken,X-CSRF-Token, Authorization, Token, EtcdID")
		c.Header("Access-Control-Allow-Methods", "POST, GET, OPTIONS, DELETE, PUT")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers, Content-Type")
		c.Header("Access-Control-Allow-Credentials", "true")

		//放行所有OPTIONS方法
		if method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		// 处理请求
		c.Next()
	}
}

func (p *Program) middlewareEtcdClient() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 过滤认证模块
		u, _ := url.ParseRequestURI(c.Request.RequestURI)
		if strings.HasPrefix(u.Path, "/v1/passport") == true || strings.HasPrefix(u.Path, "/ui") == true || strings.HasPrefix(u.Path, "/v1/upload") == true || strings.HasPrefix(u.Path, "/v1/server") == true {
			return
		}
		// 读取etcdID
		etcdId := c.GetHeader("EtcdID")
		log.Println("当前请求EtcdId", etcdId)
		if etcdId == "" {
			return
		}
		etcdIdNum, _ := strconv.Atoi(etcdId)
		if etcdIdNum == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"msg": "请选择Etcd服务",
			})
			c.Abort()
			return
		}

		// 查询角色权限 GET 请求为只读
		userinfoObj, exist := c.Get("userinfo")
		if exist == false {
			logger.Log.Warnw("用户登录信息不存在")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		userinfo := userinfoObj.(*models.UsersModel)
		// get请求为读操作
		typ := 1
		if c.Request.Method == "GET" {
			typ = 0
		}
		roleEtcdServer := new(models.RoleEtcdServersModel)
		err := roleEtcdServer.FirstByRoleIdAndEtcdServerIdAndType(userinfo.RoleId, int32(etcdIdNum), int32(typ))
		if err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(http.StatusBadRequest, gin.H{
					"msg": "无权限进行此操作",
				})
			} else {
				c.JSON(http.StatusBadRequest, gin.H{
					"msg": "数据库查询错误",
				})
			}
			c.Abort()
			return
		}

		// 查询etcd服务信息
		etcdOne := new(models.EtcdServersModel)
		etcdOne, err = etcdOne.FirstById(int32(etcdIdNum))
		if err != nil {
			logger.Log.Errorw("获取etcd服务信息错误", "EtcdID", etcdId, "err", err)
		}
		// 连接etcd
		cfg := &model.Config{
			EtcdId:    int32(etcdIdNum),
			Version:   etcdOne.Version,
			Address:   strings.Split(etcdOne.Address, ","),
			TlsEnable: etcdOne.TlsEnable == "true",
			CertFile:  etcdOne.CertFile,
			KeyFile:   etcdOne.KeyFile,
			CaFile:    etcdOne.CaFile,
			Username:  etcdOne.Username,
			Password:  etcdOne.Password,
		}
		client, err := etcdsdk.NewClientByConfig(cfg)
		if err != nil {
			logger.Log.Errorw("连接etcd服务错误", "EtcdID", etcdId, "config", cfg, "err", err)
			c.JSON(http.StatusBadRequest, gin.H{
				"msg": err.Error(),
			})
			c.Abort()
		}
		c.Set("CLIENT", client)
		// 处理请求
		c.Next()

		defer func() {
			// 请求结束关闭etcd连接
			err = client.Close()
			if err != nil {
				logger.Log.Errorw("关闭etcd连接错误", "EtcdID", etcdId, "config", cfg, "err", err)
			}
		}()
	}
}
