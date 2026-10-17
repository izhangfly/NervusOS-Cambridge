#include "nervus/gazebo/adapter.h"

#include <stddef.h>
#include <string.h>

int main(void)
{
  gz_adapter_config_t config;
  gz_adapter_error_t error;
  const gz_adapter_versions_t *versions;

  memset(&config, 0, sizeof(config));
  memset(&error, 0, sizeof(error));
  gz_adapter_config_init(&config);
  gz_adapter_error_clear(&error);
  versions = gz_adapter_versions();

  if (config.struct_size != sizeof(config))
    return 1;
  if (error.code != GZ_ADAPTER_STATUS_OK || error.message[0] != '\0')
    return 2;
  if (versions == NULL || versions->abi_version != GZ_ADAPTER_ABI_VERSION)
    return 3;
  if (strcmp(gz_adapter_status_name(GZ_ADAPTER_STATUS_TIMEOUT), "TIMEOUT") != 0)
    return 4;

  return 0;
}
