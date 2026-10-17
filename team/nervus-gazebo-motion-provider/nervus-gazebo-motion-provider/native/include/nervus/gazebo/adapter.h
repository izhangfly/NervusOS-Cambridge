#ifndef NERVUS_GAZEBO_ADAPTER_H_
#define NERVUS_GAZEBO_ADAPTER_H_

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define GZ_ADAPTER_ABI_VERSION 1u
#define GZ_ADAPTER_ERROR_MESSAGE_CAPACITY 256u

typedef struct gz_adapter gz_adapter_t;

typedef enum gz_adapter_status
{
  GZ_ADAPTER_STATUS_OK = 0,
  GZ_ADAPTER_STATUS_INVALID_ARGUMENT = 1,
  GZ_ADAPTER_STATUS_CLOSED = 2,
  GZ_ADAPTER_STATUS_NOT_FOUND = 3,
  GZ_ADAPTER_STATUS_NOT_CONNECTED = 4,
  GZ_ADAPTER_STATUS_TYPE_MISMATCH = 5,
  GZ_ADAPTER_STATUS_TIMEOUT = 6,
  GZ_ADAPTER_STATUS_REMOTE_REJECTED = 7,
  GZ_ADAPTER_STATUS_QUEUE_FULL = 8,
  GZ_ADAPTER_STATUS_ALREADY_EXISTS = 9,
  GZ_ADAPTER_STATUS_NATIVE_EXCEPTION = 10,
  GZ_ADAPTER_STATUS_INTERNAL = 11,
  GZ_ADAPTER_STATUS_MESSAGE_TOO_LARGE = 12
} gz_adapter_status_t;

typedef struct gz_adapter_error
{
  gz_adapter_status_t code;
  char message[GZ_ADAPTER_ERROR_MESSAGE_CAPACITY];
} gz_adapter_error_t;

typedef struct gz_adapter_config
{
  uint32_t struct_size;
  const char *partition;
  const char *name_space;
  size_t event_queue_capacity;
  size_t max_message_bytes;
  uint32_t publisher_connect_timeout_ms;
} gz_adapter_config_t;

typedef struct gz_adapter_versions_data
{
  uint32_t abi_version;
  const char *transport_version;
  const char *msgs_version;
} gz_adapter_versions_t;

typedef struct gz_adapter_buffer
{
  uint8_t *data;
  size_t size;
} gz_adapter_buffer_t;

typedef struct gz_adapter_string_list
{
  char **items;
  size_t count;
} gz_adapter_string_list_t;

typedef struct gz_adapter_topic_info_data
{
  size_t publisher_count;
  size_t subscriber_count;
  int type_conflict;
  char *message_type;
} gz_adapter_topic_info_t;

typedef struct gz_adapter_service_info_data
{
  size_t provider_count;
  int type_conflict;
  char *request_type;
  char *response_type;
} gz_adapter_service_info_t;

typedef struct gz_adapter_event
{
  uint64_t subscription_id;
  uint64_t sequence;
  uint64_t dropped_before;
  char *topic;
  char *message_type;
  uint8_t *data;
  size_t size;
} gz_adapter_event_t;

const gz_adapter_versions_t *gz_adapter_versions(void);
const char *gz_adapter_status_name(gz_adapter_status_t status);

void gz_adapter_config_init(gz_adapter_config_t *config);
void gz_adapter_error_clear(gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_create(
    const gz_adapter_config_t *config,
    gz_adapter_t **adapter,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_close(
    gz_adapter_t *adapter,
    gz_adapter_error_t *error);

void gz_adapter_destroy(gz_adapter_t *adapter);

gz_adapter_status_t gz_adapter_list_topics(
    gz_adapter_t *adapter,
    gz_adapter_string_list_t *topics,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_list_services(
    gz_adapter_t *adapter,
    gz_adapter_string_list_t *services,
    gz_adapter_error_t *error);

void gz_adapter_string_list_clear(gz_adapter_string_list_t *list);

gz_adapter_status_t gz_adapter_topic_info(
    gz_adapter_t *adapter,
    const char *topic,
    gz_adapter_topic_info_t *info,
    gz_adapter_error_t *error);

void gz_adapter_topic_info_clear(gz_adapter_topic_info_t *info);

gz_adapter_status_t gz_adapter_service_info(
    gz_adapter_t *adapter,
    const char *service,
    gz_adapter_service_info_t *info,
    gz_adapter_error_t *error);

void gz_adapter_service_info_clear(gz_adapter_service_info_t *info);

gz_adapter_status_t gz_adapter_wait_for_topic(
    gz_adapter_t *adapter,
    const char *topic,
    const char *expected_message_type,
    uint32_t timeout_ms,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_wait_for_subscribers(
    gz_adapter_t *adapter,
    const char *topic,
    const char *message_type,
    uint32_t timeout_ms,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_wait_for_service(
    gz_adapter_t *adapter,
    const char *service,
    const char *expected_request_type,
    const char *expected_response_type,
    uint32_t timeout_ms,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_publish(
    gz_adapter_t *adapter,
    const char *topic,
    const char *message_type,
    const uint8_t *data,
    size_t size,
    int require_connection,
    uint32_t connection_timeout_ms,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_request(
    gz_adapter_t *adapter,
    const char *service,
    const char *request_type,
    const char *response_type,
    const uint8_t *request_data,
    size_t request_size,
    uint32_t timeout_ms,
    gz_adapter_buffer_t *response,
    gz_adapter_error_t *error);

void gz_adapter_buffer_clear(gz_adapter_buffer_t *buffer);

gz_adapter_status_t gz_adapter_subscribe(
    gz_adapter_t *adapter,
    const char *topic,
    const char *message_type,
    uint64_t max_messages_per_second,
    int coalesce_latest,
    uint64_t *subscription_id,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_unsubscribe(
    gz_adapter_t *adapter,
    uint64_t subscription_id,
    gz_adapter_error_t *error);

gz_adapter_status_t gz_adapter_next_event(
    gz_adapter_t *adapter,
    uint32_t timeout_ms,
    gz_adapter_event_t *event,
    gz_adapter_error_t *error);

void gz_adapter_event_clear(gz_adapter_event_t *event);

#ifdef __cplusplus
}
#endif

#endif
