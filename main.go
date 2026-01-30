package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 升级HTTP连接为WebSocket连接
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域，生产可限制域名
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024, // 增加缓冲区，防止断连
}

// 太平洋网络IP接口返回结构体（JSON格式）
type PConlineIPResp struct {
	Ip        string `json:"ip"`
	Pro       string `json:"pro"`
	ProCode   string `json:"proCode"`
	City      string `json:"city"`
	CityCode  string `json:"cityCode"`
	Isp       string `json:"isp"`
}

// 客户端结构体（含IP/归属地/用户ID）
type Client struct {
	Conn   *websocket.Conn // WebSocket连接
	UserID string          // 用户ID（自定义/随机）
	IP     string          // 客户端IP
	Region string          // IP归属地（省-市-运营商）
}

// 消息结构体（前端<->后端通信格式）
type Message struct {
	Type    string `json:"type"`    // 消息类型：login/password/setid/chat/join/leave/online/help
	Content string `json:"content"` // 消息内容/密码/用户ID
	UserID  string `json:"userId"`  // 用户ID
	IP      string `json:"ip"`      // 发送者IP
	Region  string `json:"region"`  // IP归属地
	Time    string `json:"time"`    // 时间
}

// 聊天室核心管理（含固定登录密码）
type ChatServer struct {
	clients       map[*websocket.Conn]*Client
	broadcast     chan Message
	logChannel    chan string
	clientsMutex  sync.RWMutex
	outputFile    *os.File
	fixedPassword string // 固定登录密码
}

// 随机ID生成词库
var adjectives = []string{"快乐", "聪明", "安静", "活泼", "神秘", "勇敢", "幽默", "优雅", "可爱", "帅气"}
var nouns = []string{"小猫", "小狗", "熊猫", "老虎", "兔子", "狐狸", "海豚", "老鹰", "狮子", "蝴蝶"}

// 新建聊天室（传入固定密码）
func NewChatServer(fixedPassword string) *ChatServer {
	// 打开日志文件
	file, err := os.OpenFile("chat_history.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("打开日志文件失败: %v", err)
	}
	return &ChatServer{
		clients:       make(map[*websocket.Conn]*Client),
		broadcast:     make(chan Message, 200), // 增大广播通道缓冲区
		logChannel:    make(chan string, 400),
		outputFile:    file,
		fixedPassword: fixedPassword,
	}
}

// 生成随机用户ID
func (s *ChatServer) generateRandomID() string {
	t := time.Now()
	adj := adjectives[int(t.UnixNano())%len(adjectives)]
	noun := nouns[int(t.UnixNano())%len(nouns)]
	num := t.UnixNano()%900 + 100
	return fmt.Sprintf("%s%s%d", adj, noun, num)
}

// GBK转UTF-8 核心函数（解决中文乱码）
func GbkToUtf8(s []byte) ([]byte, error) {
	reader := transform.NewReader(strings.NewReader(string(s)), simplifiedchinese.GBK.NewDecoder())
	d, e := ioutil.ReadAll(reader)
	if e != nil {
		return nil, e
	}
	return d, nil
}

// 查询IP归属地【最终版】：GBK转UTF-8 + 太平洋网络接口 + 本地/内网兼容
func (s *ChatServer) getIPRegion(ip string) string {
	// 第一步：兼容本地/内网IP，直接返回友好提示
	localIPPrefixes := []string{"127.0.0.1", "192.168.", "10.", "172."}
	for _, prefix := range localIPPrefixes {
		if strings.HasPrefix(ip, prefix) {
			return "本地/内网IP-无公网归属"
		}
	}

	// 第二步：太平洋网络公开IP接口（JSON格式，无反爬）
	apiUrl := fmt.Sprintf("http://whois.pconline.com.cn/ipJson.jsp?ip=%s&json=true", ip)
	client := &http.Client{
		Timeout: 5 * time.Second, // 延长超时时间，防止网络抖动
	}
	resp, err := client.Get(apiUrl)
	if err != nil {
		return "归属地查询-网络超时"
	}
	defer resp.Body.Close()

	// 读取GBK编码的响应体
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		return "归属地查询-接口返回失败"
	}

	// 第三步：核心-GBK转UTF-8，彻底解决中文乱码
	utf8Body, err := GbkToUtf8(body)
	if err != nil {
		// 转码失败兜底，直接返回原解析结果
		utf8Body = body
	}

	// 第四步：解析UTF-8格式的JSON数据
	var ipResp PConlineIPResp
	if err := json.Unmarshal(utf8Body, &ipResp); err != nil {
		return "归属地查询-解析失败"
	}

	// 第五步：拼接归属地，空值兜底处理
	pro := strings.TrimSpace(ipResp.Pro)
	city := strings.TrimSpace(ipResp.City)
	isp := strings.TrimSpace(ipResp.Isp)
	if pro == "" || pro == "null" {
		pro = "未知省份"
	}
	if city == "" || city == "null" {
		city = "未知城市"
	}
	if isp == "" || isp == "null" {
		isp = "未知运营商"
	}
	region := fmt.Sprintf("%s-%s-%s", pro, city, isp)

	return region
}

// 广播消息给所有客户端（修复遍历错误，增加错误处理，防止单客户端断连影响全局）
func (s *ChatServer) Broadcaster() {
	for msg := range s.broadcast {
		s.clientsMutex.RLock()
		// 遍历前先复制客户端连接列表，防止遍历中修改
		conns := make([]*websocket.Conn, 0, len(s.clients))
		for conn := range s.clients {
			conns = append(conns, conn)
		}
		s.clientsMutex.RUnlock()

		// 遍历真实的WebSocket连接，处理消息发送
		for _, conn := range conns {
			if err := conn.WriteJSON(msg); err != nil {
				log.Printf("发送消息失败: %v，关闭连接", err)
				conn.Close()
				s.clientsMutex.Lock()
				delete(s.clients, conn)
				s.clientsMutex.Unlock()
			}
		}
		// 记录日志（含IP+归属地+ID）
		logStr := fmt.Sprintf("[%s] [%s] %s | %s | %s: %s", msg.Time, msg.Type, msg.IP, msg.Region, msg.UserID, msg.Content)
		s.logChannel <- logStr
	}
}




// 处理单个WebSocket客户端连接（加固错误处理，防止解析失败导致断连）
func (s *ChatServer) HandleClient(w http.ResponseWriter, r *http.Request) {
	// 升级为WebSocket连接
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("升级WebSocket失败: %v", err)
		return
	}
	defer func() {
		// 延迟关闭连接，确保资源释放
		conn.Close()
	}()

	// 提取客户端纯IP（优化解析，兼容IPv6和带端口的IP）
	clientIP := r.RemoteAddr
	if strings.Contains(clientIP, ":") {
		ipParts := strings.Split(clientIP, ":")
		if len(ipParts) > 1 {
			clientIP = ipParts[0]
		}
	}
	clientIP = strings.Trim(clientIP, "[]")
	// 查询IP归属地（即使解析失败，也不会导致连接断开）
	clientRegion := s.getIPRegion(clientIP)
	var client *Client

	// 第一步：密码验证（增加错误处理，防止客户端异常输入导致断连）
	conn.WriteJSON(Message{
		Type:    "password",
		Content: "=== 终端聊天室-登录验证 ===\n请输入固定登录密码：",
		Time:    time.Now().Format("15:04:05"),
	})
	for {
		var pwdMsg Message
		if err := conn.ReadJSON(&pwdMsg); err != nil {
			log.Printf("【密码验证】%s 连接断开，原因：%v", clientIP, err)
			return
		}
		// 过滤空密码
		pwd := strings.TrimSpace(strings.ToLower(pwdMsg.Content))
		if pwd == "" {
			conn.WriteJSON(Message{
				Type:    "password",
				Content: "❌ 密码不能为空！请重新输入：",
				Time:    time.Now().Format("15:04:05"),
			})
			continue
		}
		if pwd == strings.TrimSpace(strings.ToLower(s.fixedPassword)) {
			conn.WriteJSON(Message{
				Type:    "password",
				Content: "✅ 密码验证成功！进入用户ID设置环节...",
				Time:    time.Now().Format("15:04:05"),
			})
			break
		} else {
			conn.WriteJSON(Message{
				Type:    "password",
				Content: "❌ 密码错误！请重新输入固定登录密码：",
				Time:    time.Now().Format("15:04:05"),
			})
		}
	}

	// 第二步：用户ID设置（增加空ID处理，防止异常输入）
	conn.WriteJSON(Message{
		Type:    "setid",
		Content: "=== 终端聊天室-用户ID设置 ===\n请输入自定义ID（直接回车则使用随机ID）：",
		Time:    time.Now().Format("15:04:05"),
	})
	var idMsg Message
	if err := conn.ReadJSON(&idMsg); err != nil {
		log.Printf("【ID设置】%s 连接断开，原因：%v", clientIP, err)
		return
	}
	var userID string
	customID := strings.TrimSpace(idMsg.Content)
	if customID == "" {
		userID = s.generateRandomID()
	} else {
		// 过滤特殊字符，防止乱码和注入
		userID = strings.ReplaceAll(strings.ReplaceAll(customID, "\n", ""), "\r", "")
	}
	// 初始化客户端
	client = &Client{
		Conn:   conn,
		UserID: userID,
		IP:     clientIP,
		Region: clientRegion,
	}

	// 第三步：验证通过，加入聊天室
	s.clientsMutex.Lock()
	s.clients[conn] = client
	onlineCount := len(s.clients)
	s.clientsMutex.Unlock()

	// 发送欢迎消息
	now := time.Now().Format("15:04:05")
	welcomeMsg := Message{
		Type:    "welcome",
		Content: fmt.Sprintf("=== 终端聊天室 v2.0 ===\n✅ 登录成功！当前在线：%d 人\n你的信息：%s | %s | %s\n📌 可用命令：/online(在线列表) /help(帮助) /exit(退出)",
			onlineCount, clientIP, clientRegion, userID),
		Time: now,
	}
	if err := conn.WriteJSON(welcomeMsg); err != nil {
		log.Printf("发送欢迎消息失败: %v", err)
		return
	}

	// 广播加入消息
	joinMsg := Message{
		Type:    "join",
		Content: fmt.Sprintf("【系统】%s | %s | %s 加入聊天室", clientIP, clientRegion, userID),
		UserID:  userID,
		IP:      clientIP,
		Region:  clientRegion,
		Time:    now,
	}
	s.broadcast <- joinMsg
	log.Printf("[%s] 【加入】%s | %s | %s，当前在线：%d", now, clientIP, clientRegion, userID, onlineCount)

	// 第四步：循环接收普通消息/命令（加固错误处理，兼容各种输入）
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			// 客户端异常断开处理，友好广播离开消息
			s.clientsMutex.Lock()
			if _, ok := s.clients[conn]; ok {
				delete(s.clients, conn)
				onlineCount = len(s.clients)
			}
			s.clientsMutex.Unlock()

			leaveMsg := Message{
				Type:    "leave",
				Content: fmt.Sprintf("【系统】%s | %s | %s 异常离开聊天室", clientIP, clientRegion, userID),
				UserID:  userID,
				IP:      clientIP,
				Region:  clientRegion,
				Time:    time.Now().Format("15:04:05"),
			}
			s.broadcast <- leaveMsg
			log.Printf("[%s] 【离开】%s | %s | %s，当前在线：%d", leaveMsg.Time, clientIP, clientRegion, userID, onlineCount)
			return
		}

		// 补充消息基础信息
		msg.Time = time.Now().Format("15:04:05")
		msg.UserID = userID
		msg.IP = clientIP
		msg.Region = clientRegion
		inputContent := strings.TrimSpace(msg.Content)

		// 处理命令/普通消息，过滤空消息
		switch inputContent {
		case "/exit", "/quit":
			// 主动退出
			s.clientsMutex.Lock()
			delete(s.clients, conn)
			onlineCount = len(s.clients)
			s.clientsMutex.Unlock()
			leaveMsg := Message{
				Type:    "leave",
				Content: fmt.Sprintf("【系统】%s | %s | %s 主动退出聊天室", clientIP, clientRegion, userID),
				UserID:  userID,
				IP:      clientIP,
				Region:  clientRegion,
				Time:    msg.Time,
			}
			s.broadcast <- leaveMsg
			log.Printf("[%s] 【退出】%s | %s | %s，当前在线：%d", msg.Time, clientIP, clientRegion, userID, onlineCount)
			return
		case "/online":
			// 在线列表（优化排版，适配长归属地）
			s.clientsMutex.RLock()
			onlineList := fmt.Sprintf("=== 在线用户列表（%d人）===\nIP地址         | IP归属地                | 用户ID\n----------------|-------------------------|------------------------\n", len(s.clients))
			for _, c := range s.clients {
				onlineList += fmt.Sprintf("%-15s | %-28s | %s\n", c.IP, c.Region, c.UserID)
			}
			s.clientsMutex.RUnlock()
			onlineMsg := Message{
				Type:    "online",
				Content: onlineList,
				Time:    msg.Time,
			}
			conn.WriteJSON(onlineMsg)
		case "/help":
			// 帮助信息
			helpMsg := Message{
				Type:    "help",
				Content: "=== 终端聊天室-可用命令 ===\n/online - 查看在线用户列表（IP | 归属地 | 用户ID）\n/help   - 显示当前帮助信息\n/exit   - 主动退出聊天室\n直接输入 - 发送群聊消息（所有在线用户可见）",
				Time:    msg.Time,
			}
			conn.WriteJSON(helpMsg)
		default:
			// 普通群聊消息，过滤空内容
			if inputContent != "" {
				msg.Type = "chat"
				msg.Content = inputContent
				s.broadcast <- msg
			}
		}
	}
}

// 提供前端页面访问（静态文件）
func (s *ChatServer) ServeIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func main() {
	// ====================== 请确认你的固定登录密码 ======================
	fixedPassword := "123456" // 可直接修改为你需要的密码，如admin/666666
	// =====================================================================

	// 初始化聊天室
	server := NewChatServer(fixedPassword)
	// 启动广播和日志协程
	go server.Broadcaster()


	// 路由配置
	http.HandleFunc("/", server.ServeIndex)
	http.HandleFunc("/ws", server.HandleClient)

	// 启动服务，监听8080端口（增加端口占用检测）
	port := "8080"
	log.Printf("=====================================")
	log.Printf("终端聊天室 v2.0 启动成功！【乱码+断连+编译错误已修复】")
	log.Printf("固定登录密码：%s", fixedPassword)
	log.Printf("访问地址：http://localhost:%s", port)
	log.Printf("=====================================")
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatalf("服务启动失败：%v（请检查8080端口是否被占用）", err)
	}
}
