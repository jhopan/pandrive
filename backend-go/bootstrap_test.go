package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmptyDatabaseBootstrapsDefaultAdmin(t *testing.T) {
	app := newTestApp(t)
	password, err := app.ensureInitialAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(`{"email":"admin@gmail.com","password":"`+password+`"}`))
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("default admin login = %d: %s", w.Code, w.Body.String())
	}
}

func TestRegistrationIsDisabledAfterBootstrap(t *testing.T) {
	app := newTestApp(t)
	if err := app.ensureInitialAdmin(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewBufferString(`{"name":"Other","email":"other@example.test","password":"password-123"}`))
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("registration = %d: %s", w.Code, w.Body.String())
	}
}
