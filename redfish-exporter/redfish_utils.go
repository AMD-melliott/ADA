/**
 * Copyright 2024 Advanced Micro Devices, Inc.  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
**/

package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
)

type RedfishServer struct {
	IP        string `json:"ip"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	LoginType string `json:"loginType"`
	SlurmNode string `json:"slurmNode"`
}

type SubscriptionPayload struct {
	Destination         string                           `json:"Destination,omitempty"`
	EventTypes          []redfish.EventType              `json:"EventTypes,omitempty"`
	RegistryPrefixes    []string                         `json:"RegistryPrefixes,omitempty"`
	ResourceTypes       []string                         `json:"ResourceTypes,omitempty"`
	DeliveryRetryPolicy redfish.DeliveryRetryPolicy      `json:"DeliveryRetryPolicy,omitempty"`
	HTTPHeaders         map[string]string                `json:"HttpHeaders,omitempty"`
	Oem                 interface{}                      `json:"Oem,omitempty"`
	Protocol            redfish.EventDestinationProtocol `json:"Protocol,omitempty"`
	Context             string                           `json:"Context,omitempty"`
}

// Create a new connection to a redfish server
func getRedfishClient(server RedfishServer) (*gofish.APIClient, error) {
	clientConfig := gofish.ClientConfig{
		Endpoint: server.IP,
		Username: server.Username,
		Password: server.Password,
		Insecure: true, // TODO Set Based on login type
	}

	c, err := gofish.Connect(clientConfig)
	if err != nil {
		log.Printf("Error connecting to redfish server %s: %v", server.IP, err)
		return nil, err
	}

	log.Printf("Successfully connected to redfish server %s", server.IP)
	return c, nil
}

// Create a subscription
func createSubscription(server RedfishServer, SubscriptionPayload SubscriptionPayload) (string, error) {

	// Establish a connection to the server
	c, err := getRedfishClient(server)
	if err != nil {
		return "", fmt.Errorf("failed to connect to server %s: %v", server.IP, err)
	}
	defer c.Logout()

	// Get the event service
	eventService, err := c.Service.EventService()
	if err != nil {
		return "", fmt.Errorf("failed to get event service on server %s: %v", server.IP, err)
	}

	deleteConflictingSubscriptions(server, SubscriptionPayload)
	// Create the subscription based on the Redfish version
	if isV1_5() {
		return createV1_5Subscription(eventService, SubscriptionPayload)
	} else {
		return createLegacySubscription(eventService, SubscriptionPayload)
	}
}

func isV1_5() bool {
	// TODO Logic to determine if Redfish server is <v1.5 or higher
	// We assume false until we get version info on the servers.
	return false
}

// Create V1.5 subscription
func createV1_5Subscription(eventService *redfish.EventService, SubscriptionPayload SubscriptionPayload) (string, error) {
	subscriptionURI, err := eventService.CreateEventSubscriptionInstance(
		SubscriptionPayload.Destination,
		SubscriptionPayload.RegistryPrefixes,
		SubscriptionPayload.ResourceTypes,
		SubscriptionPayload.HTTPHeaders,
		SubscriptionPayload.Protocol,
		SubscriptionPayload.Context,
		SubscriptionPayload.DeliveryRetryPolicy,
		SubscriptionPayload.Oem,
	)

	if err != nil {
		return "", fmt.Errorf("failed to create v1.5 subscription: %w", err)
	}

	return subscriptionURI, nil
}

// Create legacy subscription
func createLegacySubscription(eventService *redfish.EventService, SubscriptionPayload SubscriptionPayload) (string, error) {
	subscriptionURI, err := eventService.CreateEventSubscription(
		SubscriptionPayload.Destination,
		SubscriptionPayload.EventTypes,
		SubscriptionPayload.HTTPHeaders,
		SubscriptionPayload.Protocol,
		SubscriptionPayload.Context,
		SubscriptionPayload.Oem,
	)

	if err != nil {
		return "", fmt.Errorf("failed to create legacy subscription: %w", err)
	}

	return subscriptionURI, nil
}

// Create subscriptions for all servers and return their URIs
// Rollback if any subscription attempt fails
func CreateSubscriptionsForAllServers(redfishServers []RedfishServer, subscriptionPayload SubscriptionPayload) (map[string]string, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex // to guard access to the map

	subscriptionMap := make(map[string]string)

	errChan := make(chan error, len(redfishServers))

	for _, server := range redfishServers {
		wg.Add(1)
		go func(server RedfishServer) {
			defer wg.Done()
			subscriptionURI, err := createSubscription(server, subscriptionPayload)
			if err != nil {
				errChan <- fmt.Errorf("subscription failed on server %s: %v", server.IP, err)
				return
			}
			mu.Lock()
			subscriptionMap[server.IP] = subscriptionURI
			mu.Unlock()
			log.Printf("Successfully created subscription on redfish server %s: %s", server.IP, subscriptionURI)
		}(server)
	}

	wg.Wait()
	close(errChan)

	// Any error that occurred during the subscription process
	var allErrors []string
	for err := range errChan {
		if err != nil {
			allErrors = append(allErrors, err.Error())
		}
	}

	if len(allErrors) > 0 {
		DeleteSubscriptionsFromAllServers(redfishServers, subscriptionMap)
		return nil, fmt.Errorf("subscription process encountered errors: %s", allErrors)
	}

	return subscriptionMap, nil
}

// Delete all event subscriptions stored in the map
func DeleteSubscriptionsFromAllServers(redfishServers []RedfishServer, subscriptionMap map[string]string) {
	var wg sync.WaitGroup

	log.Println("Unsubscribing from servers...")

	for serverIP, subscriptionURI := range subscriptionMap {
		wg.Add(1)
		go func(serverIP, subscriptionURI string) {
			defer wg.Done()
			server := getServerInfo(redfishServers, serverIP)
			if err := deleteSubscriptionFromServer(server, subscriptionURI); err != nil {
				log.Printf("Failed to delete event subscription on server %s: %v", server.IP, err)
			} else {
				log.Printf("Successfully deleted event subscription from server %s: %s", server.IP, subscriptionURI)
			}
		}(serverIP, subscriptionURI)
	}

	wg.Wait()
}

// Delete a subscription from a redfish server
func deleteSubscriptionFromServer(server RedfishServer, subscriptionURI string) error {

	c, err := getRedfishClient(server)
	if err != nil {
		return fmt.Errorf("failed to connect to server %s: %v", server.IP, err)
	}
	defer c.Logout()

	// Get the event service
	eventService, err := c.Service.EventService()
	if err != nil {
		return fmt.Errorf("failed to get event service on server %s: %v", server.IP, err)
	}

	// Attempt to delete the subscription
	err = eventService.DeleteEventSubscription(subscriptionURI)
	if err != nil {
		return fmt.Errorf("failed to delete event subscription on server %s: %v", server.IP, err)
	}

	return nil
}

// Unsubscribes/deletes conflicting subscriptions from the server
func deleteConflictingSubscriptions(server RedfishServer, subscriptionPayload SubscriptionPayload) error {
	subscriptions, err := getServerSubscriptions(server)
	if err != nil {
		return err
	}
	for _, subscription := range subscriptions {
		if subscription.Destination == subscriptionPayload.Destination {
			err := deleteSubscriptionFromServer(server, subscription.ODataID)
			if err != nil {
				return fmt.Errorf("failed to delete event subscription %s, on server %s: %v", subscription.ID, server.IP, err)
			} else {
				log.Printf("successfully deleted overlapping event subscription %s from server %s", subscription.ID, server.IP)
			}
		}
	}
	return nil
}

// Gets all subscriptions currently active on the given server
func getServerSubscriptions(server RedfishServer) ([]*redfish.EventDestination, error) {

	c, err := getRedfishClient(server)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server %s: %v", server.IP, err)
	}
	defer c.Logout()

	// Get the event service
	eventService, err := c.Service.EventService()
	if err != nil {
		return nil, fmt.Errorf("failed to get event service on server %s: %v", server.IP, err)
	}

	subscriptions, err := eventService.GetEventSubscriptions()
	if err != nil {
		return nil, fmt.Errorf("failed to get event subscriptions on server %s: %v", server.IP, err)
	}
	return subscriptions, nil
}

// Retrieve the server's credentials from the config based on IP
func getServerInfo(redfishServers []RedfishServer, serverIP string) RedfishServer {
	for _, redfishServer := range redfishServers {
		if redfishServer.IP == serverIP {
			return redfishServer
		}
	}
	return RedfishServer{}
}
