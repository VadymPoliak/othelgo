package server_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/armsnyder/othelgo/pkg/common"
	. "github.com/armsnyder/othelgo/pkg/server"
)

func TestServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	RegisterFailHandler(Fail)
	RunSpecs(t, "Server Suite")
}

// This is a suite of BDD-style tests for the server, using the ginkgo test framework.
//
// These tests invoke the Handler function directly.
// In order for the tests to pass, there must be a local dynamodb running.
//
// See: https://onsi.github.io/ginkgo/#getting-started-writing-your-first-test
var _ = Describe("Server", func() {
	// listen is a function that can be called to start receiving messages for a particular connection ID.
	var listen func(connID string) (messages <-chan interface{}, removeListener func())

	// Top-level setup steps which run before each test.
	BeforeEach(func() {
		useLocalDynamo()
		clearOthelgoTable()
		listen = setupMessageListener()
		log.SetOutput(GinkgoWriter) // Auto-hides server log output for passing tests
	})

	// Common test constants.
	newGameBoard := buildBoard([]move{{3, 3}, {4, 4}}, []move{{3, 4}, {4, 3}})

	Context("singleplayer game", func() {
		var client *clientConnection

		BeforeEach(func() {
			client = newClientConnection(listen)

			// Start the singleplayer game by sending a message.
			client.sendMessage(common.NewNewGameMessage(false, 0))
		})

		AfterEach(func() {
			client.close()
		})

		When("new game starts", func() {
			It("should send a new game board", func(done Done) {
				Eventually(client.receiveMessage).Should(WithTransform(getBoard, Equal(newGameBoard)))
				close(done)
			})
		})

		When("human player places a disk", func() {
			BeforeEach(func() {
				client.sendMessage(common.NewPlaceDiskMessage(1, 2, 4))
			})

			It("should change to player 2's turn and send the updated board", func(done Done) {
				expectedBoard := buildBoard([]move{{3, 3}, {4, 4}, {3, 4}, {2, 4}}, []move{{4, 3}})
				expectBoardUpdatedMessage := And(
					WithTransform(getBoard, Equal(expectedBoard)),
					WithTransform(whoseTurn, Equal(2)),
				)
				Eventually(client.receiveMessage).Should(expectBoardUpdatedMessage)
				close(done)
			})

			It("should make an AI move and send the updated board", func(done Done) {
				Eventually(client.receiveMessage).Should(And(
					WithTransform(countDisks, Equal(6)),
					WithTransform(whoseTurn, Equal(1)),
				))
				close(done)
			})
		})
	})

	Context("multiplayer game", func() {
		var host, opponent *clientConnection

		BeforeEach(func() {
			host = newClientConnection(listen)
			opponent = newClientConnection(listen)
		})

		When("host starts a new game", func() {
			BeforeEach(func() {
				host.sendMessage(common.NewNewGameMessage(true, 0))
			})

			It("should send a new game board to the host", func(done Done) {
				Eventually(host.receiveMessage).Should(WithTransform(getBoard, Equal(newGameBoard)))
				close(done)
			})

			When("opponent joins the game", func() {
				BeforeEach(func() {
					opponent.sendMessage(common.NewJoinGameMessage())
				})

				It("should send a new game board to the opponent", func(done Done) {
					Eventually(opponent.receiveMessage).Should(WithTransform(getBoard, Equal(newGameBoard)))
					close(done)
				})

				When("host makes the first move", func() {
					BeforeEach(func() {
						host.sendMessage(common.NewPlaceDiskMessage(1, 2, 4))
					})

					expectedBoard := buildBoard([]move{{3, 3}, {4, 4}, {3, 4}, {2, 4}}, []move{{4, 3}})
					expectBoardUpdatedMessage := And(
						WithTransform(getBoard, Equal(expectedBoard)),
						WithTransform(whoseTurn, Equal(2)),
					)

					It("should send the resulting board to the host", func(done Done) {
						Eventually(host.receiveMessage).Should(expectBoardUpdatedMessage)
						close(done)
					})

					It("should send the resulting board to the opponent", func(done Done) {
						Eventually(opponent.receiveMessage).Should(expectBoardUpdatedMessage)
						close(done)
					})
				})
			})
		})
	})
})

// useLocalDynamo replaces the server's real dynamodb client with a local dynamo client.
func useLocalDynamo() {
	config := aws.NewConfig().
		WithRegion("us-west-2").
		WithEndpoint("http://127.0.0.1:8042").
		WithCredentials(credentials.NewStaticCredentials("foo", "bar", ""))

	DynamoClient = dynamodb.New(session.Must(session.NewSession(config)))
}

// clearOthelgoTable deletes and recreates the othelgo dynamodb table.
func clearOthelgoTable() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, _ = DynamoClient.DeleteTableWithContext(ctx, &dynamodb.DeleteTableInput{TableName: aws.String("othelgo")})

	_, err := DynamoClient.CreateTableWithContext(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("othelgo"),
		AttributeDefinitions: []*dynamodb.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: aws.String("S")}},
		KeySchema:            []*dynamodb.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: aws.String("HASH")}},
		BillingMode:          aws.String("PAY_PER_REQUEST"),
	})

	Expect(err).NotTo(HaveOccurred(), "failed to clear dynamodb table")
}

// setupMessageListener intercepts outgoing messages from the lambda server and returns a function
// which can be invoked to receive messages for a particular connection ID.
func setupMessageListener() (listen func(connID string) (messages <-chan interface{}, removeListener func())) {
	type message struct {
		connID  string
		message interface{}
	}

	messages := make(chan message)

	// Replace the real SendMessage function, which would invoke the API Gateway Management API,
	// with an implementation that keeps messages in an in-memory messages channel.
	SendMessage = func(ctx context.Context, reqCtx events.APIGatewayWebsocketProxyRequestContext, connectionID string, msg interface{}) error {
		messages <- message{
			connID:  connectionID,
			message: msg,
		}
		return nil
	}

	// Collection of connection-id-specific listeners.
	var (
		listenersMu sync.Mutex
		listeners   = make(map[string]chan<- interface{})
	)

	// Start a background routine of routing messages to the correct connection-id-specific listener.
	go func() {
		for msg := range messages {
			listenersMu.Lock()
			listener, ok := listeners[msg.connID]
			listenersMu.Unlock()

			if !ok {
				continue
			}

			// This is non-blocking, so if the listener buffer is full the message is dropped.
			select {
			case listener <- msg.message:
			default:
			}
		}
	}()

	listen = func(connID string) (messages <-chan interface{}, removeListener func()) {
		// Create a buffered channel for messages in case the test is not ready to receive messages right away.
		c := make(chan interface{}, 100)

		// Add the new channel as a new listener.
		listenersMu.Lock()
		listeners[connID] = c
		listenersMu.Unlock()

		removeListener = func() {
			// Remove the listener.
			listenersMu.Lock()
			delete(listeners, connID)
			listenersMu.Unlock()
		}

		return c, removeListener
	}

	return listen
}

// clientConnection encapsulates the behavior of a websocket client, since in this test we invoke
// the Handler function directly instead of really using websockets.
type clientConnection struct {
	connID         string
	messages       <-chan interface{}
	removeListener func()
}

// newClientConnection creates a new clientConnection and sends a CONNECT message to Handler.
// The listen argument is a function for splitting off a new channel for receiving messages for a
// particular connection ID.
func newClientConnection(listen func(string) (messages <-chan interface{}, removeListener func())) *clientConnection {
	// Generate a random connection ID.
	var connIDSrc [8]byte
	if _, err := rand.Read(connIDSrc[:]); err != nil {
		panic(err)
	}
	connID := base64.StdEncoding.EncodeToString(connIDSrc[:])

	// setup a new channel for receiving messages for our new connection ID.
	messages, removeListener := listen(connID)

	clientConnection := &clientConnection{
		connID:         connID,
		messages:       messages,
		removeListener: removeListener,
	}

	// Send a CONNECT message before returning.
	clientConnection.sendType("CONNECT", nil)

	return clientConnection
}

// sendMessage sends a new message to Handler.
func (c *clientConnection) sendMessage(message interface{}) {
	c.sendType("MESSAGE", message)
}

// sendType sends a new message to Handler and lets you specify the message type.
func (c *clientConnection) sendType(typ string, message interface{}) {
	b, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	_, err = Handler(context.TODO(), events.APIGatewayWebsocketProxyRequest{
		Body: string(b),
		RequestContext: events.APIGatewayWebsocketProxyRequestContext{
			ConnectionID: c.connID,
			EventType:    typ,
		},
	})
	Expect(err).NotTo(HaveOccurred())
}

// receiveMessage returns the next message received by the client. It blocks until a message is
// available.
func (c *clientConnection) receiveMessage() interface{} {
	return <-c.messages
}

// close cleans up the client and sends a DISCONNECT message to Handler.
func (c *clientConnection) close() {
	c.sendType("DISCONNECT", nil)
	c.removeListener()
}

type move [2]int

func buildBoard(p1, p2 []move) (board common.Board) {
	for i, moves := range [][]move{p1, p2} {
		player := common.Disk(i + 1)

		for _, move := range moves {
			x, y := move[0], move[1]
			board[x][y] = player
		}
	}

	return board
}

// gomega matcher Transform functions, used in assertions.

func getBoard(message common.UpdateBoardMessage) common.Board {
	return message.Board
}

func countDisks(message common.UpdateBoardMessage) int {
	p1, p2 := common.KeepScore(message.Board)
	return p1 + p2
}

func whoseTurn(message common.UpdateBoardMessage) int {
	return int(message.Player)
}
