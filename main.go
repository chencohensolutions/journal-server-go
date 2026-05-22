package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
)

const defaultPort = "8080"

func main() {
	// TODO: Initialize your GraphQL resolvers
	// srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{
	// 	Resolvers: &graph.Resolver{},
	// }))

	// // Configure server
	// srv.AddTransport(transport.Options{})
	// srv.AddTransport(transport.GET{})
	// srv.AddTransport(transport.POST{})
	// srv.AddTransport(transport.MultipartForm{})

	// srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))

	// srv.Use(extension.Introspection{})
	// srv.Use(extension.AutomaticPersistedQuery{Cache: lru.New[string](100)})

	// http.Handle("/", playground.Handler("GraphQL playground", "/query"))
	// http.Handle("/query", srv)

	port := defaultPort
	log.Printf("connect to http://localhost:%s/ for GraphQL playground", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
