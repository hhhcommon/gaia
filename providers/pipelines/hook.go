package pipelines

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/labstack/echo"

	"github.com/gaia-pipeline/gaia"
	"github.com/gaia-pipeline/gaia/helper/pipelinehelper"
	"github.com/gaia-pipeline/gaia/services"
	"github.com/gaia-pipeline/gaia/workers/pipeline"
)

// Hook represent a github based webhook context.
type Hook struct {
	Signature string
	Event     string
	ID        string
	Payload   []byte
}

// Repository contains information about the repository. All we care about
// here are the possible urls for identification.
type Repository struct {
	GitURL  string `json:"git_url"`
	SSHURL  string `json:"ssh_url"`
	HTMLURL string `json:"html_url"`
}

// Payload contains information about the event like, user, commit id and so on.
// All we care about for the sake of identification is the repository.
type Payload struct {
	Repo Repository `json:"repository"`
}

func signBody(secret, body []byte) []byte {
	computed := hmac.New(sha1.New, secret)
	_, _ = computed.Write(body)
	return []byte(computed.Sum(nil))
}

func verifySignature(secret []byte, signature string, body []byte) bool {
	signaturePrefix := "sha1="
	signatureLength := 45

	if len(signature) != signatureLength || !strings.HasPrefix(signature, signaturePrefix) {
		return false
	}

	actual := make([]byte, 20)
	_, _ = hex.Decode(actual, []byte(signature[5:]))
	expected := signBody(secret, body)
	return hmac.Equal(expected, actual)
}

func parse(secret []byte, req *http.Request) (Hook, error) {
	h := Hook{}

	if h.Signature = req.Header.Get("x-hub-signature"); len(h.Signature) == 0 {
		return Hook{}, errors.New("no signature")
	}

	if h.Event = req.Header.Get("x-github-event"); len(h.Event) == 0 {
		return Hook{}, errors.New("no event")
	}

	if h.Event != "push" {
		if h.Event == "ping" {
			return Hook{Event: "ping"}, nil
		}
		return Hook{}, errors.New("invalid event")
	}

	if h.ID = req.Header.Get("x-github-delivery"); len(h.ID) == 0 {
		return Hook{}, errors.New("no event id")
	}

	body, err := ioutil.ReadAll(req.Body)

	if err != nil {
		return Hook{}, err
	}

	if !verifySignature(secret, h.Signature, body) {
		return Hook{}, errors.New("Invalid signature")
	}

	h.Payload = body
	return h, err
}

// GitWebHook handles callbacks from GitHub's webhook system.
func (pp *PipelineProvider) GitWebHook(c echo.Context) error {
	vault, err := services.DefaultVaultService()
	if err != nil {
		return c.String(http.StatusInternalServerError, "unable to initialize vault: "+err.Error())
	}

	err = vault.LoadSecrets()
	if err != nil {
		return c.String(http.StatusInternalServerError, "unable to open vault: "+err.Error())
	}

	secret, err := vault.Get("GITHUB_WEBHOOK_SECRET")
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}

	h, err := parse(secret, c.Request())
	c.Request().Header.Set("Content-type", "application/json")
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}
	if h.Event == "ping" {
		return c.NoContent(http.StatusOK)
	}

	p := Payload{}
	if err := json.Unmarshal(h.Payload, &p); err != nil {
		return c.String(http.StatusBadRequest, "error in unmarshalling json payload")
	}

	var foundPipeline *gaia.Pipeline
	for _, pipe := range pipeline.GlobalActivePipelines.GetAll() {
		if pipe.Repo.URL == p.Repo.GitURL || pipe.Repo.URL == p.Repo.HTMLURL || pipe.Repo.URL == p.Repo.SSHURL {
			foundPipeline = &pipe
			break
		}
	}
	if foundPipeline == nil {
		return c.String(http.StatusInternalServerError, "pipeline not found")
	}
	uniqueFolder, err := pipelinehelper.GetLocalDestinationForPipeline(*foundPipeline)
	if err != nil {
		gaia.Cfg.Logger.Error("Pipeline type invalid", "type", foundPipeline.Type)
		return err
	}
	foundPipeline.Repo.LocalDest = uniqueFolder
	err = pp.deps.PipelineService.UpdateRepository(foundPipeline)
	if err != nil {
		message := fmt.Sprintln("failed to build pipeline: ", err.Error())
		return c.String(http.StatusInternalServerError, message)
	}
	return c.String(http.StatusOK, "successfully processed event")
}
