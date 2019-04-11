OUT := citrus

all: build

static:
	CGO_ENABLED=0 GOOS=linux go build -o ${OUT} -a -ldflags '-extldflags "-static"' .

build:
	go build -o ${OUT}
