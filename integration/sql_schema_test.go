package integration

import (
	"testing"

	"github.com/Sirupsen/logrus"
	"github.com/ory/fosite"
	"github.com/ory/hydra/client"
	"github.com/ory/hydra/jwk"
	"github.com/ory/hydra/oauth2"
	"github.com/ory/hydra/policy"
	"github.com/ory/hydra/warden/group"
	"github.com/ory/ladon"
	"github.com/stretchr/testify/require"
)

func TestSQLSchema(t *testing.T) {
	var testGenerator = &jwk.RS256Generator{}
	ks, _ := testGenerator.Generate("")
	p1 := ks.Key("private")
	r := fosite.NewRequest()
	r.ID = "foo"
	db := ConnectToMySQL()

	cm := &client.SQLManager{DB: db, Hasher: &fosite.BCrypt{}}
	gm := group.SQLManager{DB: db}
	jm := jwk.SQLManager{DB: db, Cipher: &jwk.AEAD{Key: []byte("11111111111111111111111111111111")}}
	om := oauth2.FositeSQLStore{Manager: cm, DB: db, L: logrus.New()}
	pm, err := policy.NewSQLManager(db)
	require.Nil(t, err)

	require.Nil(t, cm.CreateSchemas())
	require.Nil(t, gm.CreateSchemas())
	require.Nil(t, jm.CreateSchemas())
	require.Nil(t, om.CreateSchemas())

	require.Nil(t, jm.AddKey("foo", jwk.First(p1)))
	require.Nil(t, pm.Create(&ladon.DefaultPolicy{ID: "foo"}))
	require.Nil(t, cm.CreateClient(&client.Client{ID: "foo"}))
	require.Nil(t, om.CreateAccessTokenSession(nil, "asdfasdf", r))
	require.Nil(t, gm.CreateGroup(&group.Group{
		ID:      "asdfas",
		Members: []string{"asdf"},
	}))
}
