// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2019 Intel Corporation

package eaa

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

// DeregisterApplication implements https API
func DeregisterApplication(w http.ResponseWriter, r *http.Request) {
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	clientCert := r.TLS.PeerCertificates[0]
	commonName := clientCert.Subject.CommonName
	URN, err := CommonNameStringToURN(commonName)
	if err != nil {
		log.Errf("Error during converting Common Name to URN: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Check preemptively if a Service exists to return the HTTP code that is more likely to be
	// correct
	statusCode := http.StatusNoContent
	if !isServicePresent(commonName, eaaCtx) {
		statusCode = http.StatusNotFound
	}

	// Prepare Service structure
	var serv Service
	serv.URN = &URN
	svcMsg := ServiceMessage{Svc: &serv, Action: serviceActionDeregister}

	// Create Watermill Message and publish it
	data, err := json.Marshal(svcMsg)
	if err != nil {
		log.Errf("Error during Service structure marshaling: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	msg := message.NewMessage(commonName, data)

	err = eaaCtx.MsgBrokerCtx.publish(servicesTopic, msg)
	if err != nil {
		log.Errf("Error during Message publishing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(statusCode)

	log.Debugf("Successfully processed DeregisterApplication from %s",
		commonName)
}

// HandleProducerNotifications is a go routine which continually anticipates
// notification messages from a Publisher application over the connected websocket
func HandleProducerNotifications(commonName string, connection *websocket.Conn, eaaCtx *Context) {
	for {
		// Read incoming message from websocket
		_, wsMsg, err := connection.ReadMessage()
		if err != nil {
			log.Debug("Stopped reading messages")
			return
		}

		// Check if application is registered as Producer
		eaaCtx.serviceInfo.RLock()
		_, serviceFound := eaaCtx.serviceInfo.m[commonName]
		eaaCtx.serviceInfo.RUnlock()
		if !serviceFound {
			log.Err("Application not registered as a Producer")
			continue
		}

		var notification NotificationFromProducer

		// Attempt to parse message as JSON
		msgBytes := bytes.NewReader(wsMsg)
		err = json.NewDecoder(msgBytes).Decode(&notification)
		if err != nil {
			log.Errf("JSON parsing error in notification: %s", err.Error())
			continue
		}

		// Generate URN structure
		URN, err := CommonNameStringToURN(commonName)
		if err != nil {
			log.Errf("Error during URN generation: %s", err.Error())
			continue
		}

		// Construct notification topic
		notifTopic := getNotificationTopicName(URN.Namespace)

		// Add a Publisher to the Notification Namespace topic (if not already)
		err = eaaCtx.MsgBrokerCtx.addPublisher(notificationPublisher, notifTopic, nil)
		if err != nil {
			// Ignore objectAlreadyExistsError error
			if _, ok := err.(objectAlreadyExistsError); !ok {
				log.Errf("Error when adding a Publisher of type: '%v', id: '%v'. Error: %s",
					notificationPublisher, notifTopic, err.Error())
				continue
			}
		}

		// Prepare a NotificationMessage that will be published via the message broker
		notifMsg := NotificationMessage{Notification: &notification, URN: &URN}

		// Create a Watermill Message
		data, err := json.Marshal(notifMsg)
		if err != nil {
			log.Errf("Error during Service structure marshaling: %s", err.Error())
			continue
		}

		// Publish message to the broker
		msgToPublish := message.NewMessage(commonName, data)
		err = eaaCtx.MsgBrokerCtx.publish(notifTopic, msgToPublish)
		if err != nil {
			log.Errf("Error while publishing message to the broker: %s", err.Error())
		}
	}
}

// CreateWebsocketConnection creates a bi-directional websocket for consumers
// to receive data from producers, and for producers to post notifications
// to subcribed consumers over a persisted websocket connection
func CreateWebsocketConnection(w http.ResponseWriter, r *http.Request) (int, error) {
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	// Get the consumer app ID from the Common Name in the certificate
	commonName := r.TLS.PeerCertificates[0].Subject.CommonName

	// Check if urn ID matches the Host included in the request header
	if commonName != r.Host {
		return http.StatusUnauthorized,
			errors.New("401: Incorrect app ID")
	}

	eaaCtx.consumerConnections.Lock()
	defer eaaCtx.consumerConnections.Unlock()

	// Check if connection was created for urn ID, if so send close
	// message, close the connection and delete the entry in the
	// connections structure
	foundConn, connFound := eaaCtx.consumerConnections.m[commonName]
	if connFound {
		prevConn := foundConn.connection
		msgType := websocket.CloseMessage
		closeMessage := websocket.FormatCloseMessage(
			websocket.CloseServiceRestart,
			"New connection request, closing this connection")
		err := prevConn.WriteMessage(msgType, closeMessage)
		if err != nil {
			log.Info("Failed to send close message to old connection")
		}
		err = prevConn.Close()
		if err != nil {
			log.Info("Failed to close previous websocket connection")
		}
		delete(eaaCtx.consumerConnections.m, commonName)
	}

	// Create nil connection obj in consumerConnections map. That means the
	// procedure of web socket connection has started.
	eaaCtx.consumerConnections.m[commonName] = ConsumerConnection{
		connection: nil}
	conn, err := socket.Upgrade(w, r, nil)
	if err != nil {
		delete(eaaCtx.consumerConnections.m, commonName)
		return 0, err
	}

	eaaCtx.consumerConnections.m[commonName] = ConsumerConnection{
		connection: conn}

	// Spawn a go routine to listen for notification messages from Producer apps
	go HandleProducerNotifications(commonName, conn, eaaCtx)

	return 0, nil
}

// GetNotifications implements https API
func GetNotifications(w http.ResponseWriter, r *http.Request) {
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	eaaCtx.serviceInfo.RLock()
	if eaaCtx.serviceInfo.m == nil {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusInternalServerError)
	}
	eaaCtx.serviceInfo.RUnlock()

	statCode, err := CreateWebsocketConnection(w, r)
	if err != nil {
		log.Errf("Error in WebSocket Connection Creation: %#v", err)
		if statCode != 0 {
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(statCode)
		}
		return
	}

	// Subscribe to the Client topic to receive all of its subscriptions
	topic := getClientTopicName(r.TLS.PeerCertificates[0].Subject.CommonName)
	err = eaaCtx.MsgBrokerCtx.addSubscriber(clientSubscriber, topic, r)
	if err != nil {
		// Ignore objectAlreadyExistsError error
		if _, ok := err.(objectAlreadyExistsError); !ok {
			log.Errf("Error when adding a Subscriber of type: '%v', topic: '%v'", clientSubscriber,
				topic)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	log.Debugf("Successfully processed GetNotifications from %s",
		r.TLS.PeerCertificates[0].Subject.CommonName)
}

// GetServices implements https API
func GetServices(w http.ResponseWriter, r *http.Request) {
	var servList ServiceList
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	eaaCtx.serviceInfo.RLock()
	defer eaaCtx.serviceInfo.RUnlock()

	if eaaCtx.serviceInfo.m == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for _, serv := range eaaCtx.serviceInfo.m {
		servList.Services = append(servList.Services, serv)
	}

	encoder := json.NewEncoder(w)
	err := encoder.Encode(servList)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	log.Debugf("Successfully processed GetServices from %s",
		r.TLS.PeerCertificates[0].Subject.CommonName)
}

// GetSubscriptions implements https API
func GetSubscriptions(w http.ResponseWriter, r *http.Request) {
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	var (
		subs       *SubscriptionList
		commonName string
		err        error
	)

	commonName = r.TLS.PeerCertificates[0].Subject.CommonName

	if subs, err = getConsumerSubscriptions(commonName, eaaCtx); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errf("Consumer Subscription List Getter: %s",
			err.Error())
		return
	}

	if err = json.NewEncoder(w).Encode(*subs); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errf("Consumer Subscription List Getter: %s",
			err.Error())
		return
	}

	log.Debugf("Successfully processed GetSubscriptions from %s", commonName)
}

// PushNotificationToSubscribers implements https API
func PushNotificationToSubscribers(w http.ResponseWriter, r *http.Request) {
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	var notif NotificationFromProducer

	err := json.NewDecoder(r.Body).Decode(&notif)
	if err != nil {
		log.Errf("Error in Publish Notification: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	commonName := r.TLS.PeerCertificates[0].Subject.CommonName
	URN, err := CommonNameStringToURN(commonName)
	if err != nil {
		log.Errf("Error during URN generation: %s", err.Error())
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Check if a Service exists
	eaaCtx.serviceInfo.RLock()
	defer eaaCtx.serviceInfo.RUnlock()

	_, serviceFound := eaaCtx.serviceInfo.m[commonName]
	if !serviceFound {
		log.Err("Producer is not registered")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	notifTopic := getNotificationTopicName(URN.Namespace)

	// Add a Publisher to the Notification Namespace topic (if not subscribed already)
	err = eaaCtx.MsgBrokerCtx.addPublisher(notificationPublisher, notifTopic, r)
	if err != nil {
		// Ignore objectAlreadyExistsError error
		if _, ok := err.(objectAlreadyExistsError); !ok {
			log.Errf("Error when adding a Publisher of type: '%v', id: '%v'. Error: %s",
				notificationPublisher, notifTopic, err.Error())
		}
	}

	// Prepare NotificationMessage that will be published using a Message Broker
	notifMsg := NotificationMessage{Notification: &notif, URN: &URN}

	// Create Watermill Message and publish it
	data, err := json.Marshal(notifMsg)
	if err != nil {
		log.Errf("Error during Service structure marshaling: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	msg := message.NewMessage(commonName, data)

	err = eaaCtx.MsgBrokerCtx.publish(notifTopic, msg)
	if err != nil {
		log.Errf("Error during Message publishing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	log.Debugf("Successfully processed PushNotificationToSubscribers from %s",
		commonName)
}

// RegisterApplication implements https API
func RegisterApplication(w http.ResponseWriter, r *http.Request) {
	var serv Service
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	clientCert := r.TLS.PeerCertificates[0]
	commonName := clientCert.Subject.CommonName

	err := json.NewDecoder(r.Body).Decode(&serv)
	if err != nil {
		log.Errf("Register Application: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Create URN from commonName
	var URN URN
	if URN, err = CommonNameStringToURN(commonName); err != nil {
		log.Errf("Error during URN generation: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	serv.URN = &URN

	// Prepare ServiceMessage that will be published using a Message Broker
	svcMsg := ServiceMessage{Svc: &serv, Action: serviceActionRegister}

	// Create Watermill Message and publish it
	data, err := json.Marshal(svcMsg)
	if err != nil {
		log.Errf("Error during Service structure marshaling: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	msg := message.NewMessage(commonName, data)

	err = eaaCtx.MsgBrokerCtx.publish(servicesTopic, msg)
	if err != nil {
		log.Errf("Error during Message publishing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	log.Debugf("Successfully processed RegisterApplication from %s",
		commonName)
}

// SubscribeNamespaceNotifications implements https API
func SubscribeNamespaceNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	var sub []NotificationDescriptor

	err := json.NewDecoder(r.Body).Decode(&sub)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errf("Namespace Notification Registration: %s",
			err.Error())
		return
	}

	commonName := r.TLS.PeerCertificates[0].Subject.CommonName

	// Get the Notification Namespace
	namespace := mux.Vars(r)["urn.namespace"]
	urn := URN{Namespace: namespace}

	err = processSubscriptionRequest(subscriptionActionSubscribe, subscriptionScopeNamespace,
		commonName, &urn, sub, r, eaaCtx)
	if err != nil {
		log.Errf("Error during Namespace Subscription Request processing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	log.Debugf("Successfully processed SubscribeNamespaceNotifications from %s",
		commonName)
}

// SubscribeServiceNotifications implements https API
func SubscribeServiceNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	var sub []NotificationDescriptor

	err := json.NewDecoder(r.Body).Decode(&sub)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errf("Service Notification Registration: %s", err.Error())
		return
	}

	commonName := r.TLS.PeerCertificates[0].Subject.CommonName

	// Get the Notification Namespace and Service ID
	vars := mux.Vars(r)
	namespace := vars["urn.namespace"]
	serviceID := vars["urn.id"]
	urn := URN{Namespace: namespace, ID: serviceID}

	err = processSubscriptionRequest(subscriptionActionSubscribe, subscriptionScopeService,
		commonName, &urn, sub, r, eaaCtx)
	if err != nil {
		log.Errf("Error during Service Subscription Request processing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	log.Debugf("Successfully processed SubscribeServiceNotifications from %s",
		commonName)
}

// UnsubscribeAllNotifications implements https API
func UnsubscribeAllNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)

	commonName := r.TLS.PeerCertificates[0].Subject.CommonName

	err := processSubscriptionRequest(subscriptionActionUnsubscribe, subscriptionScopeAll,
		commonName, nil, nil, r, eaaCtx)
	if err != nil {
		log.Errf("Error during All Unsubscription Request processing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	log.Debugf("Successfully processed UnsubscribeAllNotifications from %s",
		commonName)
}

// UnsubscribeNamespaceNotifications implements https API
func UnsubscribeNamespaceNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)
	var sub []NotificationDescriptor

	err := json.NewDecoder(r.Body).Decode(&sub)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errf("Namespace Notification Unregistration: %s",
			err.Error())
		return
	}

	commonName := r.TLS.PeerCertificates[0].Subject.CommonName

	// Get the Notification Namespace
	namespace := mux.Vars(r)["urn.namespace"]
	urn := URN{Namespace: namespace}

	err = processSubscriptionRequest(subscriptionActionUnsubscribe, subscriptionScopeNamespace,
		commonName, &urn, sub, r, eaaCtx)
	if err != nil {
		log.Errf("Error during Namespace Unsubscription Request processing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	log.Debugf("Successfully processed UnsubscribeNamespaceNotifications from"+
		"%s", commonName)
}

// UnsubscribeServiceNotifications implements https API
func UnsubscribeServiceNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	eaaCtx := r.Context().Value(contextKey("appliance-ctx")).(*Context)
	var sub []NotificationDescriptor

	err := json.NewDecoder(r.Body).Decode(&sub)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errf("Service Notification Unregistration: %s", err.Error())
		return
	}

	commonName := r.TLS.PeerCertificates[0].Subject.CommonName

	// Get the Notification Namespace and Service ID
	vars := mux.Vars(r)
	namespace := vars["urn.namespace"]
	serviceID := vars["urn.id"]
	urn := URN{Namespace: namespace, ID: serviceID}

	err = processSubscriptionRequest(subscriptionActionUnsubscribe, subscriptionScopeService,
		commonName, &urn, sub, r, eaaCtx)
	if err != nil {
		log.Errf("Error during Service Unsubscription Request processing: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	log.Debugf("Successfully processed UnsubscribeServiceNotifications from %s",
		commonName)
}

// processSubscriptionRequest adds Publisher and Subscriber to the Client topic and publishes the
// SubscriptionMessage to it.
// If subscriptionAction == subscriptionActionRegister it also subscribes to the
// Namespace Notification topic.
func processSubscriptionRequest(subscriptionAction string, subscriptionScope string,
	clientCommonName string, URN *URN, subs []NotificationDescriptor, r *http.Request,
	eaaCtx *Context) error {

	// Subscribe to the Client topic (if not subscribed already) to receive all of its subscriptions
	clientTopic := getClientTopicName(clientCommonName)
	err := eaaCtx.MsgBrokerCtx.addSubscriber(clientSubscriber, clientTopic, r)
	if err != nil {
		// Ignore objectAlreadyExistsError error
		if _, ok := err.(objectAlreadyExistsError); !ok {
			return errors.Wrapf(err, "Error when adding a Subscriber of type: '%v', topic: '%v'",
				clientSubscriber, clientTopic)
		}
	}

	if subscriptionAction == subscriptionActionSubscribe {
		// Subscribe to the Notification topic (if not subscribed already)
		if URN == nil {
			return errors.New("URN can't be nil when trying to Subscribe")
		}
		notifTopic := getNotificationTopicName(URN.Namespace)

		err = eaaCtx.MsgBrokerCtx.addSubscriber(notificationSubscriber, notifTopic, r)
		if err != nil {
			// Ignore objectAlreadyExistsError error
			if _, ok := err.(objectAlreadyExistsError); !ok {
				return errors.Wrapf(err, "Error when subscribing to Notification topic '%v'",
					notifTopic)
			}
		}
	}

	// Add a Publisher to the Client topic (if not subscribed already)
	err = eaaCtx.MsgBrokerCtx.addPublisher(clientPublisher, clientTopic, r)
	if err != nil {
		// Ignore objectAlreadyExistsError error
		if _, ok := err.(objectAlreadyExistsError); !ok {
			return errors.Wrapf(err, "Error when adding a Publisher of type: '%v', id: '%v'",
				clientPublisher, clientTopic)
		}
	}

	// Prepare the message that will be published to the Client topic
	subscription := Subscription{URN, subs}
	subscriptionMsg := SubscriptionMessage{clientCommonName, &subscription, subscriptionAction,
		subscriptionScope}

	// Create and publish the Watermill Message
	data, err := json.Marshal(subscriptionMsg)
	if err != nil {
		return errors.Wrap(err, "Error during SubscriptionMessage structure marshaling")
	}
	msg := message.NewMessage(clientCommonName, data)

	err = eaaCtx.MsgBrokerCtx.publish(clientTopic, msg)
	if err != nil {
		return errors.Wrap(err, "Error during Message publishing")
	}

	return nil
}
