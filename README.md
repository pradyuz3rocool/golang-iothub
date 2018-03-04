# golang-iothub

This repository provides both SDK and command line tools for both device-to-cloud (`iotdevice`) and cloud-to-device (`iotservice`) functionality.

This project in the active development state and if you decided to use it anyway, please vendor the source code.

Some of features are missing see [TODO](https://github.com/amenzhinsky/golang-iothub#todo).

## Examples

Send a message from a IoT device:

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/amenzhinsky/golang-iothub/iotdevice"
	"github.com/amenzhinsky/golang-iothub/iotdevice/transport/mqtt"
)

func main() {
	t, err := mqtt.New()
	if err != nil {
		log.Fatal(err)
	}
	c, err := iotdevice.NewClient(
		iotdevice.WithTransport(t),
		iotdevice.WithConnectionString(os.Getenv("DEVICE_CONNECTION_STRING")),
	)
	if err != nil {
		log.Fatal(err)
	}

	// connect to the iothub
	if err = c.Connect(context.Background(), false); err != nil {
		log.Fatal(err)
	}

	// send a device-to-cloud message
	if err = c.SendEvent(context.Background(), []byte(`hello`),
		iotdevice.WithSendProperty("a", "1"),
		iotdevice.WithSendProperty("b", "2"),
	); err != nil {
		log.Fatal(err)
	}
}
```

Receive and print messages from IoT devices in a backend application:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/amenzhinsky/golang-iothub/common"
	"github.com/amenzhinsky/golang-iothub/iotservice"
)

func main() {
	c, err := iotservice.NewClient(
		iotservice.WithConnectionString(os.Getenv("SERVICE_CONNECTION_STRING")),
	)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(c.SubscribeEvents(context.Background(), func(msg *common.Message) {
		fmt.Printf("%q sends %q", msg.ConnectionDeviceID, msg.Payload)
	}))
}
```

## CLI

There are two command line utilities: `iothub-device` and `iothub-sevice`. First is for using it on a IoT device and the second for managing and interacting with those devices. 

You can perform operations like publishing, subscribing events, registering and invoking direct method, subscribing to event feedback, etc. straight from the command line.

`iothub-service` is a [iothub-explorer](https://github.com/Azure/iothub-explorer) replacement that can be distributed as a single binary instead of need to install nodejs and add dependency hell that it brings.

See `-help` for more details.

## Testing

To enable end-to-end testing in the `tests` directory you need to set the following environment variables (hope these names are descriptive):

```
TEST_HOSTNAME
TEST_DEVICE_CONNECTION_STRING
TEST_DISABLED_DEVICE_CONNECTION_STRING
TEST_SERVICE_CONNECTION_STRING
TEST_X509_DEVICE
```

On the cloud side you need to create:

1. access policy (service connect perm)
1. disabled device (symmetric key)
1. enabled device (symmetric key)
1. enabled device (x509 self signed `443ABB6DEA8F93D5987D31D2607BE2931217752C`)

## TODO

1. Stabilize API.
1. Files uploading.
1. Batch sending.
1. HTTP transport.
1. AMQP transport.
1. AMQP-WS transport.
1. Complete set of subcommands for iothub-device and iothub-service.
1. Add missing iotservice functionality.
1. Grammar check.
1. Automated testing.
