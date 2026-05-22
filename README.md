# Go GraphQL Server

A GraphQL API server built with Go using gqlgen.

## Prerequisites

- Go 1.21 or higher
- gqlgen (install with `go get github.com/99designs/gqlgen`)

## Setup

1. **Install dependencies:**
   ```bash
   go mod download
   ```

2. **Generate GraphQL code:**
   ```bash
   go run github.com/99designs/gqlgen generate
   ```

3. **Run the server:**
   ```bash
   go run main.go
   ```

The GraphQL playground will be available at `http://localhost:8080/`

## Project Structure

```
.
├── graph/
│   ├── schema/
│   │   └── schema.graphql    # GraphQL schema definitions
│   ├── generated/            # Auto-generated GraphQL code
│   ├── model/                # Data models
│   └── resolver/             # Resolver implementations
├── main.go                   # Application entry point
├── gqlgen.yml               # gqlgen configuration
└── go.mod                   # Go module definition
```

## Development

### Add a new GraphQL type:

1. Edit `graph/schema/schema.graphql`
2. Run `go run github.com/99designs/gqlgen generate`
3. Implement resolvers in `graph/resolver/`

### Build for production:

```bash
go build -o bin/server main.go
```
