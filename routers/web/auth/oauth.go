// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package auth

import (
	stdContext "context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/models/auth"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/session"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/modules/web"
	"code.gitea.io/gitea/modules/web/middleware"
	auth_service "code.gitea.io/gitea/services/auth"
	"code.gitea.io/gitea/services/auth/source/oauth2"
	"code.gitea.io/gitea/services/externalaccount"
	"code.gitea.io/gitea/services/forms"
	user_service "code.gitea.io/gitea/services/user"

	"gitea.com/go-chi/binding"
	"github.com/golang-jwt/jwt/v4"
	"github.com/markbates/goth"
)

const (
	tplGrantAccess base.TplName = "user/auth/grant"
	tplGrantError  base.TplName = "user/auth/grant_error"
)

// TODO move error and responses to SDK or models

// AuthorizeErrorCode represents an error code specified in RFC 6749
type AuthorizeErrorCode string

const (
	// ErrorCodeInvalidRequest represents the according error in RFC 6749
	ErrorCodeInvalidRequest AuthorizeErrorCode = "invalid_request"
	// ErrorCodeUnauthorizedClient represents the according error in RFC 6749
	ErrorCodeUnauthorizedClient AuthorizeErrorCode = "unauthorized_client"
	// ErrorCodeAccessDenied represents the according error in RFC 6749
	ErrorCodeAccessDenied AuthorizeErrorCode = "access_denied"
	// ErrorCodeUnsupportedResponseType represents the according error in RFC 6749
	ErrorCodeUnsupportedResponseType AuthorizeErrorCode = "unsupported_response_type"
	// ErrorCodeInvalidScope represents the according error in RFC 6749
	ErrorCodeInvalidScope AuthorizeErrorCode = "invalid_scope"
	// ErrorCodeServerError represents the according error in RFC 6749
	ErrorCodeServerError AuthorizeErrorCode = "server_error"
	// ErrorCodeTemporaryUnavailable represents the according error in RFC 6749
	ErrorCodeTemporaryUnavailable AuthorizeErrorCode = "temporarily_unavailable"
)

// AuthorizeError represents an error type specified in RFC 6749
type AuthorizeError struct {
	ErrorCode        AuthorizeErrorCode `json:"error" form:"error"`
	ErrorDescription string
	State            string
}

// Error returns the error message
func (err AuthorizeError) Error() string {
	return fmt.Sprintf("%s: %s", err.ErrorCode, err.ErrorDescription)
}

// AccessTokenErrorCode represents an error code specified in RFC 6749
type AccessTokenErrorCode string

const (
	// AccessTokenErrorCodeInvalidRequest represents an error code specified in RFC 6749
	AccessTokenErrorCodeInvalidRequest AccessTokenErrorCode = "invalid_request"
	// AccessTokenErrorCodeInvalidClient represents an error code specified in RFC 6749
	AccessTokenErrorCodeInvalidClient = "invalid_client"
	// AccessTokenErrorCodeInvalidGrant represents an error code specified in RFC 6749
	AccessTokenErrorCodeInvalidGrant = "invalid_grant"
	// AccessTokenErrorCodeUnauthorizedClient represents an error code specified in RFC 6749
	AccessTokenErrorCodeUnauthorizedClient = "unauthorized_client"
	// AccessTokenErrorCodeUnsupportedGrantType represents an error code specified in RFC 6749
	AccessTokenErrorCodeUnsupportedGrantType = "unsupported_grant_type"
	// AccessTokenErrorCodeInvalidScope represents an error code specified in RFC 6749
	AccessTokenErrorCodeInvalidScope = "invalid_scope"
)

// AccessTokenError represents an error response specified in RFC 6749
type AccessTokenError struct {
	ErrorCode        AccessTokenErrorCode `json:"error" form:"error"`
	ErrorDescription string               `json:"error_description"`
}

// Error returns the error message
func (err AccessTokenError) Error() string {
	return fmt.Sprintf("%s: %s", err.ErrorCode, err.ErrorDescription)
}

// errCallback represents a oauth2 callback error
type errCallback struct {
	Code        string
	Description string
}

func (err errCallback) Error() string {
	return err.Description
}

// TokenType specifies the kind of token
type TokenType string

const (
	// TokenTypeBearer represents a token type specified in RFC 6749
	TokenTypeBearer TokenType = "bearer"
	// TokenTypeMAC represents a token type specified in RFC 6749
	TokenTypeMAC = "mac"
)

// AccessTokenResponse represents a successful access token response
type AccessTokenResponse struct {
	AccessToken  string    `json:"access_token"`
	TokenType    TokenType `json:"token_type"`
	ExpiresIn    int64     `json:"expires_in"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
}

func newAccessTokenResponse(ctx stdContext.Context, grant *auth.OAuth2Grant, serverKey, clientKey oauth2.JWTSigningKey) (*AccessTokenResponse, *AccessTokenError) {
	if setting.OAuth2.InvalidateRefreshTokens {
		if err := grant.IncreaseCounter(ctx); err != nil {
			return nil, &AccessTokenError{
				ErrorCode:        AccessTokenErrorCodeInvalidGrant,
				ErrorDescription: "cannot increase the grant counter",
			}
		}
	}
	// generate access token to access the API
	expirationDate := timeutil.TimeStampNow().Add(setting.OAuth2.AccessTokenExpirationTime)
	accessToken := &oauth2.Token{
		GrantID: grant.ID,
		Type:    oauth2.TypeAccessToken,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expirationDate.AsTime()),
		},
	}
	signedAccessToken, err := accessToken.SignToken(serverKey)
	if err != nil {
		return nil, &AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeInvalidRequest,
			ErrorDescription: "cannot sign token",
		}
	}

	// generate refresh token to request an access token after it expired later
	refreshExpirationDate := timeutil.TimeStampNow().Add(setting.OAuth2.RefreshTokenExpirationTime * 60 * 60).AsTime()
	refreshToken := &oauth2.Token{
		GrantID: grant.ID,
		Counter: grant.Counter,
		Type:    oauth2.TypeRefreshToken,
		RegisteredClaims: jwt.RegisteredClaims{ // nolint
			ExpiresAt: jwt.NewNumericDate(refreshExpirationDate),
		},
	}
	signedRefreshToken, err := refreshToken.SignToken(serverKey)
	if err != nil {
		return nil, &AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeInvalidRequest,
			ErrorDescription: "cannot sign token",
		}
	}

	// generate OpenID Connect id_token
	signedIDToken := ""
	if grant.ScopeContains("openid") {
		app, err := auth.GetOAuth2ApplicationByID(ctx, grant.ApplicationID)
		if err != nil {
			return nil, &AccessTokenError{
				ErrorCode:        AccessTokenErrorCodeInvalidRequest,
				ErrorDescription: "cannot find application",
			}
		}
		user, err := user_model.GetUserByID(grant.UserID)
		if err != nil {
			if user_model.IsErrUserNotExist(err) {
				return nil, &AccessTokenError{
					ErrorCode:        AccessTokenErrorCodeInvalidRequest,
					ErrorDescription: "cannot find user",
				}
			}
			log.Error("Error loading user: %v", err)
			return nil, &AccessTokenError{
				ErrorCode:        AccessTokenErrorCodeInvalidRequest,
				ErrorDescription: "server error",
			}
		}

		idToken := &oauth2.OIDCToken{
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(expirationDate.AsTime()),
				Issuer:    setting.AppURL,
				Audience:  []string{app.ClientID},
				Subject:   fmt.Sprint(grant.UserID),
			},
			Nonce: grant.Nonce,
		}
		if grant.ScopeContains("profile") {
			idToken.Name = user.FullName
			idToken.PreferredUsername = user.Name
			idToken.Profile = user.HTMLURL()
			idToken.Picture = user.AvatarLink()
			idToken.Website = user.Website
			idToken.Locale = user.Language
			idToken.UpdatedAt = user.UpdatedUnix
		}
		if grant.ScopeContains("email") {
			idToken.Email = user.Email
			idToken.EmailVerified = user.IsActive
		}
		if grant.ScopeContains("groups") {
			groups, err := getOAuthGroupsForUser(user)
			if err != nil {
				log.Error("Error getting groups: %v", err)
				return nil, &AccessTokenError{
					ErrorCode:        AccessTokenErrorCodeInvalidRequest,
					ErrorDescription: "server error",
				}
			}
			idToken.Groups = groups
		}

		signedIDToken, err = idToken.SignToken(clientKey)
		if err != nil {
			return nil, &AccessTokenError{
				ErrorCode:        AccessTokenErrorCodeInvalidRequest,
				ErrorDescription: "cannot sign token",
			}
		}
	}

	return &AccessTokenResponse{
		AccessToken:  signedAccessToken,
		TokenType:    TokenTypeBearer,
		ExpiresIn:    setting.OAuth2.AccessTokenExpirationTime,
		RefreshToken: signedRefreshToken,
		IDToken:      signedIDToken,
	}, nil
}

type userInfoResponse struct {
	Sub      string   `json:"sub"`
	Name     string   `json:"name"`
	Username string   `json:"preferred_username"`
	Email    string   `json:"email"`
	Picture  string   `json:"picture"`
	Groups   []string `json:"groups"`
}

// InfoOAuth manages request for userinfo endpoint
func InfoOAuth(ctx *context.Context) {
	if ctx.Doer == nil || ctx.Data["AuthedMethod"] != (&auth_service.OAuth2{}).Name() {
		ctx.Resp.Header().Set("WWW-Authenticate", `Bearer realm=""`)
		ctx.PlainText(http.StatusUnauthorized, "no valid authorization")
		return
	}

	response := &userInfoResponse{
		Sub:      fmt.Sprint(ctx.Doer.ID),
		Name:     ctx.Doer.FullName,
		Username: ctx.Doer.Name,
		Email:    ctx.Doer.Email,
		Picture:  ctx.Doer.AvatarLink(),
	}

	groups, err := getOAuthGroupsForUser(ctx.Doer)
	if err != nil {
		ctx.ServerError("Oauth groups for user", err)
		return
	}
	response.Groups = groups

	ctx.JSON(http.StatusOK, response)
}

// returns a list of "org" and "org:team" strings,
// that the given user is a part of.
func getOAuthGroupsForUser(user *user_model.User) ([]string, error) {
	orgs, err := models.GetUserOrgsList(user)
	if err != nil {
		return nil, fmt.Errorf("GetUserOrgList: %v", err)
	}

	var groups []string
	for _, org := range orgs {
		groups = append(groups, org.Name)
		teams, err := org.LoadTeams()
		if err != nil {
			return nil, fmt.Errorf("LoadTeams: %v", err)
		}
		for _, team := range teams {
			if team.IsMember(user.ID) {
				groups = append(groups, org.Name+":"+team.LowerName)
			}
		}
	}
	return groups, nil
}

// IntrospectOAuth introspects an oauth token
func IntrospectOAuth(ctx *context.Context) {
	if ctx.Doer == nil {
		ctx.Resp.Header().Set("WWW-Authenticate", `Bearer realm=""`)
		ctx.PlainText(http.StatusUnauthorized, "no valid authorization")
		return
	}

	var response struct {
		Active bool   `json:"active"`
		Scope  string `json:"scope,omitempty"`
		jwt.RegisteredClaims
	}

	form := web.GetForm(ctx).(*forms.IntrospectTokenForm)
	token, err := oauth2.ParseToken(form.Token, oauth2.DefaultSigningKey)
	if err == nil {
		if token.Valid() == nil {
			grant, err := auth.GetOAuth2GrantByID(ctx, token.GrantID)
			if err == nil && grant != nil {
				app, err := auth.GetOAuth2ApplicationByID(ctx, grant.ApplicationID)
				if err == nil && app != nil {
					response.Active = true
					response.Scope = grant.Scope
					response.Issuer = setting.AppURL
					response.Audience = []string{app.ClientID}
					response.Subject = fmt.Sprint(grant.UserID)
				}
			}
		}
	}

	ctx.JSON(http.StatusOK, response)
}

// AuthorizeOAuth manages authorize requests
func AuthorizeOAuth(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.AuthorizationForm)
	errs := binding.Errors{}
	errs = form.Validate(ctx.Req, errs)
	if len(errs) > 0 {
		errstring := ""
		for _, e := range errs {
			errstring += e.Error() + "\n"
		}
		ctx.ServerError("AuthorizeOAuth: Validate: ", fmt.Errorf("errors occurred during validation: %s", errstring))
		return
	}

	app, err := auth.GetOAuth2ApplicationByClientID(ctx, form.ClientID)
	if err != nil {
		if auth.IsErrOauthClientIDInvalid(err) {
			handleAuthorizeError(ctx, AuthorizeError{
				ErrorCode:        ErrorCodeUnauthorizedClient,
				ErrorDescription: "Client ID not registered",
				State:            form.State,
			}, "")
			return
		}
		ctx.ServerError("GetOAuth2ApplicationByClientID", err)
		return
	}

	user, err := user_model.GetUserByID(app.UID)
	if err != nil {
		ctx.ServerError("GetUserByID", err)
		return
	}

	if !app.ContainsRedirectURI(form.RedirectURI) {
		handleAuthorizeError(ctx, AuthorizeError{
			ErrorCode:        ErrorCodeInvalidRequest,
			ErrorDescription: "Unregistered Redirect URI",
			State:            form.State,
		}, "")
		return
	}

	if form.ResponseType != "code" {
		handleAuthorizeError(ctx, AuthorizeError{
			ErrorCode:        ErrorCodeUnsupportedResponseType,
			ErrorDescription: "Only code response type is supported.",
			State:            form.State,
		}, form.RedirectURI)
		return
	}

	// pkce support
	switch form.CodeChallengeMethod {
	case "S256":
	case "plain":
		if err := ctx.Session.Set("CodeChallengeMethod", form.CodeChallengeMethod); err != nil {
			handleAuthorizeError(ctx, AuthorizeError{
				ErrorCode:        ErrorCodeServerError,
				ErrorDescription: "cannot set code challenge method",
				State:            form.State,
			}, form.RedirectURI)
			return
		}
		if err := ctx.Session.Set("CodeChallengeMethod", form.CodeChallenge); err != nil {
			handleAuthorizeError(ctx, AuthorizeError{
				ErrorCode:        ErrorCodeServerError,
				ErrorDescription: "cannot set code challenge",
				State:            form.State,
			}, form.RedirectURI)
			return
		}
		// Here we're just going to try to release the session early
		if err := ctx.Session.Release(); err != nil {
			// we'll tolerate errors here as they *should* get saved elsewhere
			log.Error("Unable to save changes to the session: %v", err)
		}
	case "":
		break
	default:
		handleAuthorizeError(ctx, AuthorizeError{
			ErrorCode:        ErrorCodeInvalidRequest,
			ErrorDescription: "unsupported code challenge method",
			State:            form.State,
		}, form.RedirectURI)
		return
	}

	grant, err := app.GetGrantByUserID(ctx, ctx.Doer.ID)
	if err != nil {
		handleServerError(ctx, form.State, form.RedirectURI)
		return
	}

	// Redirect if user already granted access
	if grant != nil {
		code, err := grant.GenerateNewAuthorizationCode(ctx, form.RedirectURI, form.CodeChallenge, form.CodeChallengeMethod)
		if err != nil {
			handleServerError(ctx, form.State, form.RedirectURI)
			return
		}
		redirect, err := code.GenerateRedirectURI(form.State)
		if err != nil {
			handleServerError(ctx, form.State, form.RedirectURI)
			return
		}
		// Update nonce to reflect the new session
		if len(form.Nonce) > 0 {
			err := grant.SetNonce(ctx, form.Nonce)
			if err != nil {
				log.Error("Unable to update nonce: %v", err)
			}
		}
		ctx.Redirect(redirect.String())
		return
	}

	// show authorize page to grant access
	ctx.Data["Application"] = app
	ctx.Data["RedirectURI"] = form.RedirectURI
	ctx.Data["State"] = form.State
	ctx.Data["Scope"] = form.Scope
	ctx.Data["Nonce"] = form.Nonce
	ctx.Data["ApplicationUserLinkHTML"] = "<a href=\"" + html.EscapeString(user.HTMLURL()) + "\">@" + html.EscapeString(user.Name) + "</a>"
	ctx.Data["ApplicationRedirectDomainHTML"] = "<strong>" + html.EscapeString(form.RedirectURI) + "</strong>"
	// TODO document SESSION <=> FORM
	err = ctx.Session.Set("client_id", app.ClientID)
	if err != nil {
		handleServerError(ctx, form.State, form.RedirectURI)
		log.Error(err.Error())
		return
	}
	err = ctx.Session.Set("redirect_uri", form.RedirectURI)
	if err != nil {
		handleServerError(ctx, form.State, form.RedirectURI)
		log.Error(err.Error())
		return
	}
	err = ctx.Session.Set("state", form.State)
	if err != nil {
		handleServerError(ctx, form.State, form.RedirectURI)
		log.Error(err.Error())
		return
	}
	// Here we're just going to try to release the session early
	if err := ctx.Session.Release(); err != nil {
		// we'll tolerate errors here as they *should* get saved elsewhere
		log.Error("Unable to save changes to the session: %v", err)
	}
	ctx.HTML(http.StatusOK, tplGrantAccess)
}

// GrantApplicationOAuth manages the post request submitted when a user grants access to an application
func GrantApplicationOAuth(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.GrantApplicationForm)
	if ctx.Session.Get("client_id") != form.ClientID || ctx.Session.Get("state") != form.State ||
		ctx.Session.Get("redirect_uri") != form.RedirectURI {
		ctx.Error(http.StatusBadRequest)
		return
	}
	app, err := auth.GetOAuth2ApplicationByClientID(ctx, form.ClientID)
	if err != nil {
		ctx.ServerError("GetOAuth2ApplicationByClientID", err)
		return
	}
	grant, err := app.CreateGrant(ctx, ctx.Doer.ID, form.Scope)
	if err != nil {
		handleAuthorizeError(ctx, AuthorizeError{
			State:            form.State,
			ErrorDescription: "cannot create grant for user",
			ErrorCode:        ErrorCodeServerError,
		}, form.RedirectURI)
		return
	}
	if len(form.Nonce) > 0 {
		err := grant.SetNonce(ctx, form.Nonce)
		if err != nil {
			log.Error("Unable to update nonce: %v", err)
		}
	}

	var codeChallenge, codeChallengeMethod string
	codeChallenge, _ = ctx.Session.Get("CodeChallenge").(string)
	codeChallengeMethod, _ = ctx.Session.Get("CodeChallengeMethod").(string)

	code, err := grant.GenerateNewAuthorizationCode(ctx, form.RedirectURI, codeChallenge, codeChallengeMethod)
	if err != nil {
		handleServerError(ctx, form.State, form.RedirectURI)
		return
	}
	redirect, err := code.GenerateRedirectURI(form.State)
	if err != nil {
		handleServerError(ctx, form.State, form.RedirectURI)
		return
	}
	ctx.Redirect(redirect.String(), http.StatusSeeOther)
}

// OIDCWellKnown generates JSON so OIDC clients know Gitea's capabilities
func OIDCWellKnown(ctx *context.Context) {
	t := ctx.Render.TemplateLookup("user/auth/oidc_wellknown")
	ctx.Resp.Header().Set("Content-Type", "application/json")
	ctx.Data["SigningKey"] = oauth2.DefaultSigningKey
	if err := t.Execute(ctx.Resp, ctx.Data); err != nil {
		log.Error("%v", err)
		ctx.Error(http.StatusInternalServerError)
	}
}

// OIDCKeys generates the JSON Web Key Set
func OIDCKeys(ctx *context.Context) {
	jwk, err := oauth2.DefaultSigningKey.ToJWK()
	if err != nil {
		log.Error("Error converting signing key to JWK: %v", err)
		ctx.Error(http.StatusInternalServerError)
		return
	}

	jwk["use"] = "sig"

	jwks := map[string][]map[string]string{
		"keys": {
			jwk,
		},
	}

	ctx.Resp.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(ctx.Resp)
	if err := enc.Encode(jwks); err != nil {
		log.Error("Failed to encode representation as json. Error: %v", err)
	}
}

// AccessTokenOAuth manages all access token requests by the client
func AccessTokenOAuth(ctx *context.Context) {
	form := *web.GetForm(ctx).(*forms.AccessTokenForm)
	if form.ClientID == "" {
		authHeader := ctx.Req.Header.Get("Authorization")
		authContent := strings.SplitN(authHeader, " ", 2)
		if len(authContent) == 2 && authContent[0] == "Basic" {
			payload, err := base64.StdEncoding.DecodeString(authContent[1])
			if err != nil {
				handleAccessTokenError(ctx, AccessTokenError{
					ErrorCode:        AccessTokenErrorCodeInvalidRequest,
					ErrorDescription: "cannot parse basic auth header",
				})
				return
			}
			pair := strings.SplitN(string(payload), ":", 2)
			if len(pair) != 2 {
				handleAccessTokenError(ctx, AccessTokenError{
					ErrorCode:        AccessTokenErrorCodeInvalidRequest,
					ErrorDescription: "cannot parse basic auth header",
				})
				return
			}
			form.ClientID = pair[0]
			form.ClientSecret = pair[1]
		}
	}

	serverKey := oauth2.DefaultSigningKey
	clientKey := serverKey
	if serverKey.IsSymmetric() {
		var err error
		clientKey, err = oauth2.CreateJWTSigningKey(serverKey.SigningMethod().Alg(), []byte(form.ClientSecret))
		if err != nil {
			handleAccessTokenError(ctx, AccessTokenError{
				ErrorCode:        AccessTokenErrorCodeInvalidRequest,
				ErrorDescription: "Error creating signing key",
			})
			return
		}
	}

	switch form.GrantType {
	case "refresh_token":
		handleRefreshToken(ctx, form, serverKey, clientKey)
	case "authorization_code":
		handleAuthorizationCode(ctx, form, serverKey, clientKey)
	default:
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnsupportedGrantType,
			ErrorDescription: "Only refresh_token or authorization_code grant type is supported",
		})
	}
}

func handleRefreshToken(ctx *context.Context, form forms.AccessTokenForm, serverKey, clientKey oauth2.JWTSigningKey) {
	token, err := oauth2.ParseToken(form.RefreshToken, serverKey)
	if err != nil {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnauthorizedClient,
			ErrorDescription: "client is not authorized",
		})
		return
	}
	// get grant before increasing counter
	grant, err := auth.GetOAuth2GrantByID(ctx, token.GrantID)
	if err != nil || grant == nil {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeInvalidGrant,
			ErrorDescription: "grant does not exist",
		})
		return
	}

	// check if token got already used
	if setting.OAuth2.InvalidateRefreshTokens && (grant.Counter != token.Counter || token.Counter == 0) {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnauthorizedClient,
			ErrorDescription: "token was already used",
		})
		log.Warn("A client tried to use a refresh token for grant_id = %d was used twice!", grant.ID)
		return
	}
	accessToken, tokenErr := newAccessTokenResponse(ctx, grant, serverKey, clientKey)
	if tokenErr != nil {
		handleAccessTokenError(ctx, *tokenErr)
		return
	}
	ctx.JSON(http.StatusOK, accessToken)
}

func handleAuthorizationCode(ctx *context.Context, form forms.AccessTokenForm, serverKey, clientKey oauth2.JWTSigningKey) {
	app, err := auth.GetOAuth2ApplicationByClientID(ctx, form.ClientID)
	if err != nil {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeInvalidClient,
			ErrorDescription: fmt.Sprintf("cannot load client with client id: '%s'", form.ClientID),
		})
		return
	}
	if !app.ValidateClientSecret([]byte(form.ClientSecret)) {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnauthorizedClient,
			ErrorDescription: "client is not authorized",
		})
		return
	}
	if form.RedirectURI != "" && !app.ContainsRedirectURI(form.RedirectURI) {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnauthorizedClient,
			ErrorDescription: "client is not authorized",
		})
		return
	}
	authorizationCode, err := auth.GetOAuth2AuthorizationByCode(ctx, form.Code)
	if err != nil || authorizationCode == nil {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnauthorizedClient,
			ErrorDescription: "client is not authorized",
		})
		return
	}
	// check if code verifier authorizes the client, PKCE support
	if !authorizationCode.ValidateCodeChallenge(form.CodeVerifier) {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeUnauthorizedClient,
			ErrorDescription: "client is not authorized",
		})
		return
	}
	// check if granted for this application
	if authorizationCode.Grant.ApplicationID != app.ID {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeInvalidGrant,
			ErrorDescription: "invalid grant",
		})
		return
	}
	// remove token from database to deny duplicate usage
	if err := authorizationCode.Invalidate(ctx); err != nil {
		handleAccessTokenError(ctx, AccessTokenError{
			ErrorCode:        AccessTokenErrorCodeInvalidRequest,
			ErrorDescription: "cannot proceed your request",
		})
	}
	resp, tokenErr := newAccessTokenResponse(ctx, authorizationCode.Grant, serverKey, clientKey)
	if tokenErr != nil {
		handleAccessTokenError(ctx, *tokenErr)
		return
	}
	// send successful response
	ctx.JSON(http.StatusOK, resp)
}

func handleAccessTokenError(ctx *context.Context, acErr AccessTokenError) {
	ctx.JSON(http.StatusBadRequest, acErr)
}

func handleServerError(ctx *context.Context, state, redirectURI string) {
	handleAuthorizeError(ctx, AuthorizeError{
		ErrorCode:        ErrorCodeServerError,
		ErrorDescription: "A server error occurred",
		State:            state,
	}, redirectURI)
}

func handleAuthorizeError(ctx *context.Context, authErr AuthorizeError, redirectURI string) {
	if redirectURI == "" {
		log.Warn("Authorization failed: %v", authErr.ErrorDescription)
		ctx.Data["Error"] = authErr
		ctx.HTML(http.StatusBadRequest, tplGrantError)
		return
	}
	redirect, err := url.Parse(redirectURI)
	if err != nil {
		ctx.ServerError("url.Parse", err)
		return
	}
	q := redirect.Query()
	q.Set("error", string(authErr.ErrorCode))
	q.Set("error_description", authErr.ErrorDescription)
	q.Set("state", authErr.State)
	redirect.RawQuery = q.Encode()
	ctx.Redirect(redirect.String(), http.StatusSeeOther)
}

// SignInOAuth handles the OAuth2 login buttons
func SignInOAuth(ctx *context.Context) {
	provider := ctx.Params(":provider")

	authSource, err := auth.GetActiveOAuth2SourceByName(provider)
	if err != nil {
		ctx.ServerError("SignIn", err)
		return
	}

	// try to do a direct callback flow, so we don't authenticate the user again but use the valid accesstoken to get the user
	user, gothUser, err := oAuth2UserLoginCallback(authSource, ctx.Req, ctx.Resp)
	if err == nil && user != nil {
		// we got the user without going through the whole OAuth2 authentication flow again
		handleOAuth2SignIn(ctx, authSource, user, gothUser)
		return
	}

	if err = authSource.Cfg.(*oauth2.Source).Callout(ctx.Req, ctx.Resp); err != nil {
		if strings.Contains(err.Error(), "no provider for ") {
			if err = oauth2.ResetOAuth2(); err != nil {
				ctx.ServerError("SignIn", err)
				return
			}
			if err = authSource.Cfg.(*oauth2.Source).Callout(ctx.Req, ctx.Resp); err != nil {
				ctx.ServerError("SignIn", err)
			}
			return
		}
		ctx.ServerError("SignIn", err)
	}
	// redirect is done in oauth2.Auth
}

// SignInOAuthCallback handles the callback from the given provider
func SignInOAuthCallback(ctx *context.Context) {
	provider := ctx.Params(":provider")

	// first look if the provider is still active
	authSource, err := auth.GetActiveOAuth2SourceByName(provider)
	if err != nil {
		ctx.ServerError("SignIn", err)
		return
	}

	if authSource == nil {
		ctx.ServerError("SignIn", errors.New("No valid provider found, check configured callback url in provider"))
		return
	}

	u, gothUser, err := oAuth2UserLoginCallback(authSource, ctx.Req, ctx.Resp)
	if err != nil {
		if user_model.IsErrUserProhibitLogin(err) {
			uplerr := err.(user_model.ErrUserProhibitLogin)
			log.Info("Failed authentication attempt for %s from %s: %v", uplerr.Name, ctx.RemoteAddr(), err)
			ctx.Data["Title"] = ctx.Tr("auth.prohibit_login")
			ctx.HTML(http.StatusOK, "user/auth/prohibit_login")
			return
		}
		if callbackErr, ok := err.(errCallback); ok {
			log.Info("Failed OAuth callback: (%v) %v", callbackErr.Code, callbackErr.Description)
			switch callbackErr.Code {
			case "access_denied":
				ctx.Flash.Error(ctx.Tr("auth.oauth.signin.error.access_denied"))
			case "temporarily_unavailable":
				ctx.Flash.Error(ctx.Tr("auth.oauth.signin.error.temporarily_unavailable"))
			default:
				ctx.Flash.Error(ctx.Tr("auth.oauth.signin.error"))
			}
			ctx.Redirect(setting.AppSubURL + "/user/login")
			return
		}
		ctx.ServerError("UserSignIn", err)
		return
	}

	if u == nil {
		if !setting.Service.AllowOnlyInternalRegistration && setting.OAuth2Client.EnableAutoRegistration {
			// create new user with details from oauth2 provider
			var missingFields []string
			if gothUser.UserID == "" {
				missingFields = append(missingFields, "sub")
			}
			if gothUser.Email == "" {
				missingFields = append(missingFields, "email")
			}
			if setting.OAuth2Client.Username == setting.OAuth2UsernameNickname && gothUser.NickName == "" {
				missingFields = append(missingFields, "nickname")
			}
			if len(missingFields) > 0 {
				log.Error("OAuth2 Provider %s returned empty or missing fields: %s", authSource.Name, missingFields)
				if authSource.IsOAuth2() && authSource.Cfg.(*oauth2.Source).Provider == "openidConnect" {
					log.Error("You may need to change the 'OPENID_CONNECT_SCOPES' setting to request all required fields")
				}
				err = fmt.Errorf("OAuth2 Provider %s returned empty or missing fields: %s", authSource.Name, missingFields)
				ctx.ServerError("CreateUser", err)
				return
			}
			u = &user_model.User{
				Name:        getUserName(&gothUser),
				FullName:    gothUser.Name,
				Email:       gothUser.Email,
				LoginType:   auth.OAuth2,
				LoginSource: authSource.ID,
				LoginName:   gothUser.UserID,
			}

			overwriteDefault := &user_model.CreateUserOverwriteOptions{
				IsActive: util.OptionalBoolOf(!setting.OAuth2Client.RegisterEmailConfirm),
			}

			setUserGroupClaims(authSource, u, &gothUser)

			if !createAndHandleCreatedUser(ctx, base.TplName(""), nil, u, overwriteDefault, &gothUser, setting.OAuth2Client.AccountLinking != setting.OAuth2AccountLinkingDisabled) {
				// error already handled
				return
			}
		} else {
			// no existing user is found, request attach or new account
			showLinkingLogin(ctx, gothUser)
			return
		}
	}

	handleOAuth2SignIn(ctx, authSource, u, gothUser)
}

func claimValueToStringSlice(claimValue interface{}) []string {
	var groups []string

	switch rawGroup := claimValue.(type) {
	case []string:
		groups = rawGroup
	case []interface{}:
		for _, group := range rawGroup {
			groups = append(groups, fmt.Sprintf("%s", group))
		}
	default:
		str := fmt.Sprintf("%s", rawGroup)
		groups = strings.Split(str, ",")
	}
	return groups
}

func setUserGroupClaims(loginSource *auth.Source, u *user_model.User, gothUser *goth.User) bool {
	source := loginSource.Cfg.(*oauth2.Source)
	if source.GroupClaimName == "" || (source.AdminGroup == "" && source.RestrictedGroup == "") {
		return false
	}

	groupClaims, has := gothUser.RawData[source.GroupClaimName]
	if !has {
		return false
	}

	groups := claimValueToStringSlice(groupClaims)

	wasAdmin, wasRestricted := u.IsAdmin, u.IsRestricted

	if source.AdminGroup != "" {
		u.IsAdmin = false
	}
	if source.RestrictedGroup != "" {
		u.IsRestricted = false
	}

	for _, g := range groups {
		if source.AdminGroup != "" && g == source.AdminGroup {
			u.IsAdmin = true
		} else if source.RestrictedGroup != "" && g == source.RestrictedGroup {
			u.IsRestricted = true
		}
	}

	return wasAdmin != u.IsAdmin || wasRestricted != u.IsRestricted
}

func showLinkingLogin(ctx *context.Context, gothUser goth.User) {
	if _, err := session.RegenerateSession(ctx.Resp, ctx.Req); err != nil {
		ctx.ServerError("RegenerateSession", err)
		return
	}

	if err := ctx.Session.Set("linkAccountGothUser", gothUser); err != nil {
		log.Error("Error setting linkAccountGothUser in session: %v", err)
	}
	if err := ctx.Session.Release(); err != nil {
		log.Error("Error storing session: %v", err)
	}
	ctx.Redirect(setting.AppSubURL + "/user/link_account")
}

func updateAvatarIfNeed(url string, u *user_model.User) {
	if setting.OAuth2Client.UpdateAvatar && len(url) > 0 {
		resp, err := http.Get(url)
		if err == nil {
			defer func() {
				_ = resp.Body.Close()
			}()
		}
		// ignore any error
		if err == nil && resp.StatusCode == http.StatusOK {
			data, err := io.ReadAll(io.LimitReader(resp.Body, setting.Avatar.MaxFileSize+1))
			if err == nil && int64(len(data)) <= setting.Avatar.MaxFileSize {
				_ = user_service.UploadAvatar(u, data)
			}
		}
	}
}

func handleOAuth2SignIn(ctx *context.Context, source *auth.Source, u *user_model.User, gothUser goth.User) {
	updateAvatarIfNeed(gothUser.AvatarURL, u)

	needs2FA := false
	if !source.Cfg.(*oauth2.Source).SkipLocalTwoFA {
		_, err := auth.GetTwoFactorByUID(u.ID)
		if err != nil && !auth.IsErrTwoFactorNotEnrolled(err) {
			ctx.ServerError("UserSignIn", err)
			return
		}
		needs2FA = err == nil
	}

	// If this user is enrolled in 2FA and this source doesn't override it,
	// we can't sign the user in just yet. Instead, redirect them to the 2FA authentication page.
	if !needs2FA {
		if _, err := session.RegenerateSession(ctx.Resp, ctx.Req); err != nil {
			ctx.ServerError("RegenerateSession", err)
			return
		}

		if err := ctx.Session.Set("uid", u.ID); err != nil {
			log.Error("Error setting uid in session: %v", err)
		}
		if err := ctx.Session.Set("uname", u.Name); err != nil {
			log.Error("Error setting uname in session: %v", err)
		}
		if err := ctx.Session.Release(); err != nil {
			log.Error("Error storing session: %v", err)
		}

		// Clear whatever CSRF cookie has right now, force to generate a new one
		middleware.DeleteCSRFCookie(ctx.Resp)

		// Register last login
		u.SetLastLogin()

		// Update GroupClaims
		changed := setUserGroupClaims(source, u, &gothUser)
		cols := []string{"last_login_unix"}
		if changed {
			cols = append(cols, "is_admin", "is_restricted")
		}

		if err := user_model.UpdateUserCols(ctx, u, cols...); err != nil {
			ctx.ServerError("UpdateUserCols", err)
			return
		}

		// update external user information
		if err := externalaccount.UpdateExternalUser(u, gothUser); err != nil {
			log.Error("UpdateExternalUser failed: %v", err)
		}

		if err := resetLocale(ctx, u); err != nil {
			ctx.ServerError("resetLocale", err)
			return
		}

		if redirectTo := ctx.GetCookie("redirect_to"); len(redirectTo) > 0 {
			middleware.DeleteRedirectToCookie(ctx.Resp)
			ctx.RedirectToFirst(redirectTo)
			return
		}

		ctx.Redirect(setting.AppSubURL + "/")
		return
	}

	changed := setUserGroupClaims(source, u, &gothUser)
	if changed {
		if err := user_model.UpdateUserCols(ctx, u, "is_admin", "is_restricted"); err != nil {
			ctx.ServerError("UpdateUserCols", err)
			return
		}
	}

	if _, err := session.RegenerateSession(ctx.Resp, ctx.Req); err != nil {
		ctx.ServerError("RegenerateSession", err)
		return
	}

	// User needs to use 2FA, save data and redirect to 2FA page.
	if err := ctx.Session.Set("twofaUid", u.ID); err != nil {
		log.Error("Error setting twofaUid in session: %v", err)
	}
	if err := ctx.Session.Set("twofaRemember", false); err != nil {
		log.Error("Error setting twofaRemember in session: %v", err)
	}
	if err := ctx.Session.Release(); err != nil {
		log.Error("Error storing session: %v", err)
	}

	// If WebAuthn is enrolled -> Redirect to WebAuthn instead
	regs, err := auth.GetWebAuthnCredentialsByUID(u.ID)
	if err == nil && len(regs) > 0 {
		ctx.Redirect(setting.AppSubURL + "/user/webauthn")
		return
	}

	ctx.Redirect(setting.AppSubURL + "/user/two_factor")
}

// OAuth2UserLoginCallback attempts to handle the callback from the OAuth2 provider and if successful
// login the user
func oAuth2UserLoginCallback(authSource *auth.Source, request *http.Request, response http.ResponseWriter) (*user_model.User, goth.User, error) {
	oauth2Source := authSource.Cfg.(*oauth2.Source)

	gothUser, err := oauth2Source.Callback(request, response)
	if err != nil {
		if err.Error() == "securecookie: the value is too long" || strings.Contains(err.Error(), "Data too long") {
			log.Error("OAuth2 Provider %s returned too long a token. Current max: %d. Either increase the [OAuth2] MAX_TOKEN_LENGTH or reduce the information returned from the OAuth2 provider", authSource.Name, setting.OAuth2.MaxTokenLength)
			err = fmt.Errorf("OAuth2 Provider %s returned too long a token. Current max: %d. Either increase the [OAuth2] MAX_TOKEN_LENGTH or reduce the information returned from the OAuth2 provider", authSource.Name, setting.OAuth2.MaxTokenLength)
		}
		// goth does not provide the original error message
		// https://github.com/markbates/goth/issues/348
		if strings.Contains(err.Error(), "server response missing access_token") || strings.Contains(err.Error(), "could not find a matching session for this request") {
			errorCode := request.FormValue("error")
			errorDescription := request.FormValue("error_description")
			if errorCode != "" || errorDescription != "" {
				return nil, goth.User{}, errCallback{
					Code:        errorCode,
					Description: errorDescription,
				}
			}
		}
		return nil, goth.User{}, err
	}

	if oauth2Source.RequiredClaimName != "" {
		claimInterface, has := gothUser.RawData[oauth2Source.RequiredClaimName]
		if !has {
			return nil, goth.User{}, user_model.ErrUserProhibitLogin{Name: gothUser.UserID}
		}

		if oauth2Source.RequiredClaimValue != "" {
			groups := claimValueToStringSlice(claimInterface)
			found := false
			for _, group := range groups {
				if group == oauth2Source.RequiredClaimValue {
					found = true
					break
				}
			}
			if !found {
				return nil, goth.User{}, user_model.ErrUserProhibitLogin{Name: gothUser.UserID}
			}
		}
	}

	user := &user_model.User{
		LoginName:   gothUser.UserID,
		LoginType:   auth.OAuth2,
		LoginSource: authSource.ID,
	}

	hasUser, err := user_model.GetUser(user)
	if err != nil {
		return nil, goth.User{}, err
	}

	if hasUser {
		return user, gothUser, nil
	}

	// search in external linked users
	externalLoginUser := &user_model.ExternalLoginUser{
		ExternalID:    gothUser.UserID,
		LoginSourceID: authSource.ID,
	}
	hasUser, err = user_model.GetExternalLogin(externalLoginUser)
	if err != nil {
		return nil, goth.User{}, err
	}
	if hasUser {
		user, err = user_model.GetUserByID(externalLoginUser.UserID)
		return user, gothUser, err
	}

	// no user found to login
	return nil, gothUser, nil
}
