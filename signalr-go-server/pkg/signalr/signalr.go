package signalr

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

type Hub struct {
	context HubContext
}

func (h *Hub) Initialize(ctx HubContext) {
	h.context = ctx
}

func (h *Hub) Clients() HubClients {
	return h.context.Clients()
}

func (h *Hub) Groups() GroupManager {
	return h.context.Groups()
}

func (h *Hub) OnConnected(connectionID string) {

}
func (h *Hub) OnDisconnected(connectionID string) {

}

type HubInterface interface {
	Initialize(hubContext HubContext)
	OnConnected(connectionID string)
	OnDisconnected(connectionID string)
}

type HubLifetimeManager interface {
	OnConnected(conn hubConnection)
	OnDisconnected(conn hubConnection)
	InvokeAll(target string, args []interface{})
	InvokeClient(connectionID string, target string, args []interface{})
	InvokeGroup(groupName string, target string, args []interface{})
	AddToGroup(groupName, connectionID string)
	RemoveFromGroup(groupName, connectionID string)
}

type ClientProxy interface {
	Send(target string, args ...interface{})
}

type HubClients interface {
	All() ClientProxy
	Client(connectionID string) ClientProxy
	Group(groupName string) ClientProxy
}

type GroupManager interface {
	AddToGroup(groupName string, connectionID string)
	RemoveFromGroup(groupName string, connectionID string)
}

type HubContext interface {
	Clients() HubClients
	Groups() GroupManager
}

// HubProtocol interface
type HubProtocol interface {
	ReadMessage(buf *bytes.Buffer) (interface{}, error)
	WriteMessage(message interface{}, writer io.Writer) error
}

// Implementation

type jsonHubProtocol struct {
}

func (j *jsonHubProtocol) ReadMessage(buf *bytes.Buffer) (interface{}, error) {
	data, err := parseTextMessageFormat(buf)
	if err != nil {
		return nil, err
	}

	message := hubMessage{}
	err = json.Unmarshal(data, &message)

	if err != nil {
		return nil, err
	}

	switch message.Type {
	case 1:
		invocation := hubInvocationMessage{}
		err = json.Unmarshal(data, &invocation)
		return invocation, err
	default:
		return message, nil
	}
}

func (j *jsonHubProtocol) WriteMessage(message interface{}, writer io.Writer) error {

	// TODO: Reduce the amount of copies

	// We're copying because we want to write complete messages to the underlying writer
	buf := bytes.Buffer{}

	if err := json.NewEncoder(&buf).Encode(message); err != nil {
		return err
	}

	if err := buf.WriteByte(30); err != nil {
		return err
	}

	_, err := writer.Write(buf.Bytes())
	return err
}

type defaultHubContext struct {
	clients HubClients
	groups  GroupManager
}

func (d *defaultHubContext) Clients() HubClients {
	return d.clients
}

func (d *defaultHubContext) Groups() GroupManager {
	return d.groups
}

type defaultHubLifetimeManager struct {
	clients sync.Map
	groups  sync.Map
}

func (d *defaultHubLifetimeManager) OnConnected(conn hubConnection) {
	d.clients.Store(conn.getConnectionID(), conn)
}

func (d *defaultHubLifetimeManager) OnDisconnected(conn hubConnection) {
	d.clients.Delete(conn.getConnectionID())
}

func (d *defaultHubLifetimeManager) InvokeAll(target string, args []interface{}) {
	d.clients.Range(func(key, value interface{}) bool {
		value.(hubConnection).sendInvocation(target, args)
		return true
	})
}

func (d *defaultHubLifetimeManager) InvokeClient(connectionID string, target string, args []interface{}) {
	client, ok := d.clients.Load(connectionID)

	if !ok {
		return
	}

	client.(hubConnection).sendInvocation(target, args)
}

func (d *defaultHubLifetimeManager) InvokeGroup(groupName string, target string, args []interface{}) {
	groups, ok := d.groups.Load(groupName)

	if !ok {
		// No such group
		return
	}

	for _, v := range groups.(map[string]hubConnection) {
		v.sendInvocation(target, args)
	}
}

func (d *defaultHubLifetimeManager) AddToGroup(groupName string, connectionID string) {
	client, ok := d.clients.Load(connectionID)

	if !ok {
		// No such client
		return
	}

	groups, _ := d.groups.LoadOrStore(groupName, make(map[string]hubConnection))

	groups.(map[string]hubConnection)[connectionID] = client.(hubConnection)
}

func (d *defaultHubLifetimeManager) RemoveFromGroup(groupName string, connectionID string) {
	groups, ok := d.groups.Load(groupName)

	if !ok {
		return
	}

	delete(groups.(map[string]hubConnection), connectionID)
}

type defaultGroupManager struct {
	lifetimeManager HubLifetimeManager
}

func (d *defaultGroupManager) AddToGroup(groupName string, connectionID string) {
	d.lifetimeManager.AddToGroup(groupName, connectionID)
}

func (d *defaultGroupManager) RemoveFromGroup(groupName string, connectionID string) {
	d.lifetimeManager.RemoveFromGroup(groupName, connectionID)
}

type hubInfo struct {
	hub             HubInterface
	lifetimeManager HubLifetimeManager
	methods         map[string]reflect.Value
}

type allClientProxy struct {
	lifetimeManager HubLifetimeManager
}

func (a *allClientProxy) Send(target string, args ...interface{}) {
	a.lifetimeManager.InvokeAll(target, args)
}

type singleClientProxy struct {
	connectionID    string
	lifetimeManager HubLifetimeManager
}

func (a *singleClientProxy) Send(target string, args ...interface{}) {
	a.lifetimeManager.InvokeClient(a.connectionID, target, args)
}

type groupClientProxy struct {
	groupName       string
	lifetimeManager HubLifetimeManager
}

func (g *groupClientProxy) Send(target string, args ...interface{}) {
	g.lifetimeManager.InvokeGroup(g.groupName, target, args)
}

type defaultHubClients struct {
	lifetimeManager HubLifetimeManager
	allCache        allClientProxy
}

func (c *defaultHubClients) All() ClientProxy {
	return &c.allCache
}

func (c *defaultHubClients) Client(connectionID string) ClientProxy {
	return &singleClientProxy{connectionID: connectionID, lifetimeManager: c.lifetimeManager}
}

func (c *defaultHubClients) Group(groupName string) ClientProxy {
	return &groupClientProxy{groupName: groupName, lifetimeManager: c.lifetimeManager}
}

// Protocol
type hubMessage struct {
	Type int `json:"type"`
}

type hubInvocationMessage struct {
	Type         int               `json:"type"`
	Target       string            `json:"target"`
	InvocationID string            `json:"invocationId"`
	Arguments    []json.RawMessage `json:"arguments"`
}

type sendOnlyHubInvocationMessage struct {
	Type      int           `json:"type"`
	Target    string        `json:"target"`
	Arguments []interface{} `json:"arguments"`
}

type completionMessage struct {
	Type         int         `json:"type"`
	InvocationID string      `json:"invocationId"`
	Result       interface{} `json:"result"`
	Error        string      `json:"error"`
}

type closeMessage struct {
	Type           int    `json:"type"`
	Error          string `json:"error"`
	AllowReconnect bool   `json:"allowReconnect"`
}

type handshakeRequest struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
}

type hubConnection interface {
	isConnected() bool
	getConnectionID() string
	sendInvocation(target string, args []interface{})
	completion(id string, result interface{}, error string)
	ping()
}

type webSocketHubConnection struct {
	ws           *websocket.Conn
	protocol     HubProtocol
	connectionID string
	connected    int32
}

func (w *webSocketHubConnection) start() {
	atomic.CompareAndSwapInt32(&w.connected, 0, 1)
}

func (w *webSocketHubConnection) isConnected() bool {
	return atomic.LoadInt32(&w.connected) == 1
}

func (w *webSocketHubConnection) getConnectionID() string {
	return w.connectionID
}

func (w *webSocketHubConnection) sendInvocation(target string, args []interface{}) {
	var invocationMessage = sendOnlyHubInvocationMessage{
		Type:      1,
		Target:    target,
		Arguments: args,
	}

	w.protocol.WriteMessage(invocationMessage, w.ws)
}

func (w *webSocketHubConnection) completion(id string, result interface{}, error string) {
	var completionMessage = completionMessage{
		Type:         3,
		InvocationID: id,
		Result:       result,
		Error:        error,
	}

	w.protocol.WriteMessage(completionMessage, w.ws)
}

func (w *webSocketHubConnection) ping() {
	var pingMessage = hubMessage{
		Type: 6,
	}

	w.protocol.WriteMessage(pingMessage, w.ws)
}

func (w *webSocketHubConnection) close(error string) {
	atomic.StoreInt32(&w.connected, 0)

	var closeMessage = closeMessage{
		Type:           6,
		Error:          error,
		AllowReconnect: true,
	}

	w.protocol.WriteMessage(closeMessage, w.ws)
}

func processHandshake(ws *websocket.Conn, buf *bytes.Buffer) (HubProtocol, error) {
	var err error
	var data []byte
	var protocol HubProtocol
	var ok bool
	const handshakeResponse = "{}\u001e"
	const errorHandshakeResponse = "{\"error\":\"%s\"}\u001e"

	// 5 seconds to process the handshake
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	for {
		if err = websocket.Message.Receive(ws, &data); err != nil {
			break
		}

		buf.Write(data)

		rawHandshake, err := parseTextMessageFormat(buf)

		if err != nil {
			// Partial message, read more data
			continue
		}

		fmt.Println("Handshake received")

		request := handshakeRequest{}
		err = json.Unmarshal(rawHandshake, &request)

		if err != nil {
			// Malformed handshake
			break
		}

		protocol, ok = protocolMap[request.Protocol]

		if ok {
			// Send the handshake response
			err = websocket.Message.Send(ws, handshakeResponse)
		} else {
			// Protocol not supported
			fmt.Printf("\"%s\" is the only supported protocol\n", request.Protocol)
			err = websocket.Message.Send(ws, fmt.Sprintf(errorHandshakeResponse, fmt.Sprintf("Protocol \"%s\" not supported", request.Protocol)))
		}
		break
	}

	// Disable the timeout (either we already timeout out or)
	ws.SetReadDeadline(time.Time{})

	return protocol, err
}

func hubConnectionHandler(connectionID string, ws *websocket.Conn, hubInfo *hubInfo) {
	var err error
	var data []byte
	var waitgroup sync.WaitGroup
	var buf bytes.Buffer

	protocol, err := processHandshake(ws, &buf)

	if err != nil {
		fmt.Println(err)
		return
	}

	conn := webSocketHubConnection{protocol: protocol, connectionID: connectionID, ws: ws}
	conn.start()

	hubInfo.lifetimeManager.OnConnected(&conn)
	hubInfo.hub.OnConnected(connectionID)

	// Start sending pings to the client
	waitgroup.Add(1)
	go pingLoop(&waitgroup, &conn)

	for conn.isConnected() {
		for {
			message, err := protocol.ReadMessage(&buf)

			if err != nil {
				// Partial message, need more data
				break
			}

			switch message.(type) {
			case hubInvocationMessage:
				invocation := message.(hubInvocationMessage)

				// Dispatch invocation here
				normalized := strings.ToLower(invocation.Target)

				method, ok := hubInfo.methods[normalized]

				if !ok {
					// Unable to find the method
					conn.completion(invocation.InvocationID, nil, fmt.Sprintf("Unknown method %s", invocation.Target))
					break
				}

				in := make([]reflect.Value, method.Type().NumIn())

				for i := 0; i < method.Type().NumIn(); i++ {
					t := method.Type().In(i)
					arg := reflect.New(t)
					json.Unmarshal(invocation.Arguments[i], arg.Interface())
					in[i] = arg.Elem()
				}

				result := method.Call(in)

				if len(result) > 0 {
					// REVIEW: When is this ever > 1
					conn.completion(invocation.InvocationID, result[0].Interface(), "")
				} else {
					conn.completion(invocation.InvocationID, nil, "")
				}

				break
			case hubMessage:
				// Ping
				break
			}
		}

		if err = websocket.Message.Receive(ws, &data); err != nil {
			fmt.Println(err)
			break
		}

		// Main message loop
		fmt.Println("Message received " + string(data))

		buf.Write(data)
	}

	hubInfo.hub.OnDisconnected(connectionID)
	hubInfo.lifetimeManager.OnDisconnected(&conn)
	conn.close("")

	// Wait for pings to complete
	waitgroup.Wait()
}

func parseTextMessageFormat(buf *bytes.Buffer) ([]byte, error) {
	// 30 = ASCII record separator
	data, err := buf.ReadBytes(30)

	if err != nil {
		return data, err
	}
	// Remove the delimeter
	return data[0 : len(data)-1], err
}

func pingLoop(waitGroup *sync.WaitGroup, conn hubConnection) {
	defer waitGroup.Done()

	for conn.isConnected() {
		conn.ping()
		time.Sleep(5 * time.Second)
	}
}

type availableTransport struct {
	Transport       string   `json:"transport"`
	TransferFormats []string `json:"transferFormats"`
}

type negotiateResponse struct {
	ConnectionID        string               `json:"connectionId"`
	AvailableTransports []availableTransport `json:"availableTransports"`
}

func negotiateHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.WriteHeader(400)
		return
	}

	connectionID := getConnectionID()

	response := negotiateResponse{
		ConnectionID: connectionID,
		AvailableTransports: []availableTransport{
			availableTransport{
				Transport:       "WebSockets",
				TransferFormats: []string{"Text", "Binary"},
			},
		},
	}

	json.NewEncoder(w).Encode(response)
}

func getConnectionID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return base64.StdEncoding.EncodeToString(bytes)
}

var protocolMap map[string]HubProtocol = make(map[string]HubProtocol)
var once sync.Once

// MapHub used to register a SignalR Hub with the specified ServeMux
func MapHub(mux *http.ServeMux, path string, hub HubInterface) {

	once.Do(func() {
		protocolMap["json"] = &jsonHubProtocol{}
	})

	lifetimeManager := defaultHubLifetimeManager{}
	hubClients := defaultHubClients{
		lifetimeManager: &lifetimeManager,
		allCache:        allClientProxy{lifetimeManager: &lifetimeManager},
	}

	groupManager := defaultGroupManager{
		lifetimeManager: &lifetimeManager,
	}

	hubContext := defaultHubContext{
		clients: &hubClients,
		groups:  &groupManager,
	}

	hubInfo := hubInfo{
		hub:             hub,
		lifetimeManager: &lifetimeManager,
		methods:         make(map[string]reflect.Value),
	}

	hub.Initialize(&hubContext)

	hubType := reflect.TypeOf(hub)
	hubValue := reflect.ValueOf(hub)

	for i := 0; i < hubType.NumMethod(); i++ {
		m := hubType.Method(i)
		hubInfo.methods[strings.ToLower(m.Name)] = hubValue.Method(i)
	}

	mux.HandleFunc(fmt.Sprintf("%s/negotiate", path), negotiateHandler)
	mux.Handle(path, websocket.Handler(func(ws *websocket.Conn) {
		connectionID := ws.Request().URL.Query().Get("id")

		if len(connectionID) == 0 {
			// Support websocket connection without negotiate
			connectionID = getConnectionID()
		}

		hubConnectionHandler(connectionID, ws, &hubInfo)
	}))
}
