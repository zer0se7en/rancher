package tokens

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rancher/norman/types"
	"github.com/rancher/rancher/pkg/features"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/wrangler/pkg/randomtoken"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

type DummyIndexer struct {
	cache.Store
}

type TestCase struct {
	token   string
	userID  string
	receive bool
	err     string
}

var (
	token       string
	tokenHashed string
)

// TestTokenStreamTransformer validates that the function properly filters data in websocket
func TestTokenStreamTransformer(t *testing.T) {
	features.TokenHashing.Set(true)

	assert := assert.New(t)

	tokenManager := Manager{
		tokenIndexer: &DummyIndexer{
			&cache.FakeCustomStore{},
		},
	}

	apiCtx := &types.APIContext{
		Request: &http.Request{},
	}

	var err error
	token, err = randomtoken.Generate()
	if err != nil {
		assert.FailNow(fmt.Sprintf("unable to generate token for token stream transformer test: %v", err))
	}
	tokenHashed, err = CreateSHA256Hash(token)
	if err != nil {
		assert.FailNow(fmt.Sprintf("unable to hash token for token stream transformer test: %v", err))
	}

	testCases := []TestCase{
		{
			token:   "testname:" + token,
			userID:  "testuser",
			receive: true,
			err:     "",
		},
		{
			token:   "testname:testtoken",
			userID:  "testuser",
			receive: false,
			err:     "Invalid auth token value",
		},
		{
			token:   "wrongname:testkey",
			userID:  "testuser",
			receive: false,
			err:     "422: [TokenStreamTransformer] failed: Invalid auth token value",
		},
		{
			token:   "testname:wrongkey",
			userID:  "testname",
			receive: false,
			err:     "422: [TokenStreamTransformer] failed: Invalid auth token value",
		},
		{
			token:   "testname:" + token,
			userID:  "diffname",
			receive: false,
			err:     "",
		},
		{
			token:   "",
			userID:  "testuser",
			receive: false,
			err:     "401: [TokenStreamTransformer] failed: No valid token cookie or auth header",
		},
	}

	for index, testCase := range testCases {
		failureMessage := fmt.Sprintf("test case #%d failed", index)

		dataStream := make(chan map[string]interface{}, 1)
		dataReceived := make(chan bool, 1)

		apiCtx.Request.Header = map[string][]string{"Authorization": {fmt.Sprintf("Bearer %s", testCase.token)}}

		df, err := tokenManager.TokenStreamTransformer(apiCtx, nil, dataStream, nil)
		if testCase.err == "" {
			assert.Nil(err, failureMessage)
		} else {
			assert.NotNil(err, failureMessage)
			assert.Contains(err.Error(), testCase.err, failureMessage)
		}

		ticker := time.NewTicker(1 * time.Second)
		go receivedData(df, ticker.C, dataReceived)

		// test data is received when data stream contains matching userID
		dataStream <- map[string]interface{}{"labels": map[string]interface{}{UserIDLabel: testCase.userID}}
		assert.Equal(<-dataReceived, testCase.receive)
		close(dataStream)
		ticker.Stop()
	}
}

func receivedData(c <-chan map[string]interface{}, t <-chan time.Time, result chan<- bool) {
	select {
	case <-c:
		result <- true
	case <-t:
		// assume data will not be received after 1 second timeout
		result <- false
	}
}

func (d *DummyIndexer) Index(indexName string, obj interface{}) ([]interface{}, error) {
	return nil, nil
}

func (d *DummyIndexer) IndexKeys(indexName, indexKey string) ([]string, error) {
	return []string{}, nil
}

func (d *DummyIndexer) ListIndexFuncValues(indexName string) []string {
	return []string{}
}

func (d *DummyIndexer) ByIndex(indexName, indexKey string) ([]interface{}, error) {
	return []interface{}{
		&v3.Token{
			Token: tokenHashed,
			ObjectMeta: v1.ObjectMeta{
				Name: "testname",
			},
			UserID: "testuser",
		},
	}, nil
}

func (d *DummyIndexer) GetIndexers() cache.Indexers {
	return nil
}

func (d *DummyIndexer) AddIndexers(newIndexers cache.Indexers) error {
	return nil
}
