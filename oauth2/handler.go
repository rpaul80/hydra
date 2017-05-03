package oauth2

import (
	"net/http"
	"net/url"

	"encoding/json"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/sessions"
	"github.com/julienschmidt/httprouter"
	"github.com/ory/fosite"
	"github.com/ory/herodot"
	"github.com/ory/hydra/pkg"
	"github.com/pkg/errors"
)

const (
	OpenIDConnectKeyName = "hydra.openid.id-token"

	ConsentPath = "/oauth2/consent"
	TokenPath   = "/oauth2/token"
	AuthPath    = "/oauth2/auth"

	// IntrospectPath points to the OAuth2 introspection endpoint.
	IntrospectPath = "/oauth2/introspect"
	RevocationPath = "/oauth2/revoke"

	consentCookieName = "consent_session"
)

type Handler struct {
	OAuth2  fosite.OAuth2Provider
	Consent ConsentStrategy

	H herodot.Writer

	ForcedHTTP bool
	ConsentURL url.URL

	AccessTokenLifespan time.Duration
	CookieStore         sessions.Store

	L logrus.FieldLogger
}

func (h *Handler) SetRoutes(r *httprouter.Router) {
	r.POST(TokenPath, h.TokenHandler)
	r.GET(AuthPath, h.AuthHandler)
	r.POST(AuthPath, h.AuthHandler)
	r.GET(ConsentPath, h.DefaultConsentHandler)
	r.POST(IntrospectPath, h.IntrospectHandler)
	r.POST(RevocationPath, h.RevocationHandler)
}

func (h *Handler) RevocationHandler(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var ctx = fosite.NewContext()

	err := h.OAuth2.NewRevocationRequest(ctx, r)
	if err != nil {
		pkg.LogError(err, h.L)
	}

	h.OAuth2.WriteRevocationResponse(w, err)
}

func (h *Handler) IntrospectHandler(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var session = NewSession("")

	var ctx = fosite.NewContext()
	resp, err := h.OAuth2.NewIntrospectionRequest(ctx, r, session)
	if err != nil {
		pkg.LogError(err, h.L)
		h.OAuth2.WriteIntrospectionError(w, err)
		return
	}

	exp := resp.GetAccessRequester().GetSession().GetExpiresAt(fosite.AccessToken)
	if exp.IsZero() {
		exp = resp.GetAccessRequester().GetRequestedAt().Add(h.AccessTokenLifespan)
	}

	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	err = json.NewEncoder(w).Encode(&Introspection{
		Active:    true,
		ClientID:  resp.GetAccessRequester().GetClient().GetID(),
		Scope:     strings.Join(resp.GetAccessRequester().GetGrantedScopes(), " "),
		ExpiresAt: exp.Unix(),
		IssuedAt:  resp.GetAccessRequester().GetRequestedAt().Unix(),
		Subject:   resp.GetAccessRequester().GetSession().GetSubject(),
		Username:  resp.GetAccessRequester().GetSession().GetUsername(),
		Extra:     resp.GetAccessRequester().GetSession().(*Session).Extra,
		Audience:  resp.GetAccessRequester().GetClient().GetID(),
	})
	if err != nil {
		pkg.LogError(err, h.L)
	}
}

func (h *Handler) TokenHandler(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var session = NewSession("")
	var ctx = fosite.NewContext()

	accessRequest, err := h.OAuth2.NewAccessRequest(ctx, r, session)
	if err != nil {
		pkg.LogError(err, h.L)
		h.OAuth2.WriteAccessError(w, accessRequest, err)
		return
	}

	if accessRequest.GetGrantTypes().Exact("client_credentials") {
		session.Subject = accessRequest.GetClient().GetID()
		for _, scope := range accessRequest.GetRequestedScopes() {
			if fosite.HierarchicScopeStrategy(accessRequest.GetClient().GetScopes(), scope) {
				accessRequest.GrantScope(scope)
			}
		}
	}

	accessResponse, err := h.OAuth2.NewAccessResponse(ctx, r, accessRequest)
	if err != nil {
		pkg.LogError(err, h.L)
		h.OAuth2.WriteAccessError(w, accessRequest, err)
		return
	}

	h.OAuth2.WriteAccessResponse(w, accessRequest, accessResponse)
}

func (h *Handler) AuthHandler(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var ctx = fosite.NewContext()

	authorizeRequest, err := h.OAuth2.NewAuthorizeRequest(ctx, r)
	if err != nil {
		pkg.LogError(err, h.L)
		h.writeAuthorizeError(w, authorizeRequest, err)
		return
	}

	// A session_token will be available if the user was authenticated an gave consent
	consentToken := authorizeRequest.GetRequestForm().Get("consent")
	if consentToken == "" {
		// otherwise redirect to log in endpoint
		if err := h.redirectToConsent(w, r, authorizeRequest); err != nil {
			pkg.LogError(err, h.L)
			h.writeAuthorizeError(w, authorizeRequest, err)
			return
		}
		return
	}

	cookie, err := h.CookieStore.Get(r, consentCookieName)
	if err != nil {
		pkg.LogError(err, h.L)
		h.writeAuthorizeError(w, authorizeRequest, errors.Wrapf(fosite.ErrServerError, "Could not open session: %s", err))
		return
	}

	// decode consent_token claims
	// verify anti-CSRF (inject state) and anti-replay token (expiry time, good value would be 10 seconds)
	session, err := h.Consent.ValidateResponse(authorizeRequest, consentToken, cookie)
	if err != nil {
		pkg.LogError(err, h.L)
		h.writeAuthorizeError(w, authorizeRequest, errors.Wrap(fosite.ErrAccessDenied, ""))
		return
	}

	if err := cookie.Save(r, w); err != nil {
		pkg.LogError(err, h.L)
		h.writeAuthorizeError(w, authorizeRequest, errors.Wrapf(fosite.ErrServerError, "Could not store session cookie: %s", err))
		return
	}

	// done
	response, err := h.OAuth2.NewAuthorizeResponse(ctx, r, authorizeRequest, session)
	if err != nil {
		pkg.LogError(err, h.L)
		h.writeAuthorizeError(w, authorizeRequest, err)
		return
	}

	h.OAuth2.WriteAuthorizeResponse(w, authorizeRequest, response)
}

func (h *Handler) redirectToConsent(w http.ResponseWriter, r *http.Request, authorizeRequest fosite.AuthorizeRequester) error {
	schema := "https"
	if h.ForcedHTTP {
		schema = "http"
	}

	// Error can be ignored because a session will always be returned
	cookie, _ := h.CookieStore.Get(r, consentCookieName)

	challenge, err := h.Consent.IssueChallenge(authorizeRequest, schema+"://"+r.Host+r.URL.String(), cookie)
	if err != nil {
		return err
	}

	p := h.ConsentURL
	q := p.Query()
	q.Set("challenge", challenge)
	p.RawQuery = q.Encode()

	if err := cookie.Save(r, w); err != nil {
		return err
	}

	http.Redirect(w, r, p.String(), http.StatusFound)
	return nil
}

func (h *Handler) writeAuthorizeError(w http.ResponseWriter, ar fosite.AuthorizeRequester, err error) {
	if !ar.IsRedirectURIValid() {
		var rfcerr = fosite.ErrorToRFC6749Error(err)

		redirectURI := h.ConsentURL
		query := redirectURI.Query()
		query.Add("error", rfcerr.Name)
		query.Add("error_description", rfcerr.Description)
		redirectURI.RawQuery = query.Encode()

		w.Header().Add("Location", redirectURI.String())
		w.WriteHeader(http.StatusFound)
		return
	}

	h.OAuth2.WriteAuthorizeError(w, ar, err)
}
