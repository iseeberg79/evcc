import type { DeviceType, MODBUS_COMSET, MeterTemplateUsage } from "@/types/evcc";
import { ConfigType } from "@/types/evcc";
import api from "@/api";
import { extractPlaceholders, replacePlaceholders } from "@/utils/placeholder";

export type Product = {
  group: string;
  name: string;
  template: string;
};

export type Template = {
  Params: TemplateParam[];
  Auth?: {
    type: string;
    params?: string[];
  };
  Requirements: {
    Description: string;
  };
};

export type TemplateParamUsage = "vehicle" | "battery" | "grid" | "pv" | "charger" | "aux" | "ext";

export type ServiceConfig = {
  endpoint?: string;
  params?: Record<string, any>;
  dependencies?: string[][];
};

export type TemplateParam = {
  Name: string;
  Required: boolean;
  Advanced: boolean;
  Deprecated: boolean;
  Default?: string | number | boolean;
  Choice?: string[];
  Service?: string | ServiceConfig;
  Usages?: TemplateParamUsage[];
};

export type ParamService = {
  name: string;
  dependencies: string[];
  dependencyGroups?: string[][];
  url: (values: Record<string, any>) => string;
};

export type ModbusCapability = "rs485" | "tcpip";

export type ModbusParam = TemplateParam & {
  ID?: string;
  Comset?: MODBUS_COMSET;
  Baudrate?: number;
  Port?: number;
};

export type DeviceValues = {
  type: ConfigType;
  icon?: string;
  deviceProduct?: string;
  yaml?: string;
  template: string | null;
  deviceTitle?: string;
  deviceIcon?: string;
  usage?: MeterTemplateUsage;
  heating?: boolean;
  integrateddevice?: boolean;
  stationid?: string;
  [key: string]: any;
};

export type ApiData = {
  type?: ConfigType;
  icon?: string;
  usage?: MeterTemplateUsage;
  title?: string;
  identifiers?: string[];
  [key: string]: any;
};

export type AuthCheckResponse = {
  success: boolean;
  error?: string;
  authId?: string;
};

export function handleError(e: any, msg: string) {
  console.error(e);
  let message = msg;
  const { error } = e.response.data || {};
  if (error) message += `: ${error}`;
  alert(message);
}

export function applyDefaultsFromTemplate(template: Template | null, values: DeviceValues) {
  const params = template?.Params || [];
  params
    .filter((p) => p.Default && !values[p.Name])
    .forEach((p) => {
      values[p.Name] = p.Default;
    });

  // Apply modbus defaults from template (for service dependency resolution)
  const modbusParam = params.find((p) => p.Name === "modbus") as ModbusParam | undefined;
  if (modbusParam) {
    if (!values["id"] && modbusParam.ID) {
      values["id"] = modbusParam.ID;
    }
    if (!values["port"] && modbusParam.Port) {
      values["port"] = modbusParam.Port;
    }
    if (!values["comset"] && modbusParam.Comset) {
      values["comset"] = modbusParam.Comset;
    }
    if (!values["baudrate"] && modbusParam.Baudrate) {
      values["baudrate"] = modbusParam.Baudrate;
    }
  }
}

export function customChargerName(type: ConfigType, isHeating: boolean) {
  if (!type) {
    return "config.general.customOption";
  }
  const prefix = "config.charger.type.";
  const suffix = isHeating ? ".heating" : ".charging";
  if (type === ConfigType.Custom) {
    return `${prefix}custom${suffix}`;
  }
  return `${prefix}${type}`;
}

export async function loadServiceValues(path: string) {
  try {
    const response = await api.get(`/config/service/${path}`, {
      validateStatus: (status) => status >= 200 && status < 500,
    });
    return (response.data as string[]) || [];
  } catch {
    return [];
  }
}

export const createServiceEndpoints = (params: TemplateParam[]): ParamService[] => {
  return params
    .map((param) => {
      if (!param.Service) {
        return null;
      }

      const stringValues = (values: Record<string, any>): Record<string, string> =>
        Object.entries(values).reduce(
          (acc, [key, val]) => {
            if (val !== undefined && val !== null) acc[key] = String(val);
            return acc;
          },
          {} as Record<string, string>
        );

      // Handle string shorthand: "hardware/serial"
      if (typeof param.Service === "string") {
        return {
          name: param.Name,
          dependencies: extractPlaceholders(param.Service),
          url: (values: Record<string, any>) =>
            replacePlaceholders(param.Service as string, stringValues(values)),
        } as ParamService;
      }

      // Handle object format with params/dependencies
      const serviceConfig = param.Service as ServiceConfig;

      // Extract placeholders from all param values
      const extractDeps = serviceConfig.params
        ? Object.values(serviceConfig.params).flatMap((v) =>
            extractPlaceholders(String(v))
          )
        : [];

      // Simple placeholder substitution without encoding (URLSearchParams handles encoding)
      const substitutePlaceholders = (template: string, vals: Record<string, string>): string =>
        template.replace(/\{(\w+)\}/g, (match, key) => vals[key] ?? match);

      return {
        name: param.Name,
        dependencies: extractDeps,
        dependencyGroups: serviceConfig.dependencies,
        url: (values: Record<string, any>) => {
          if (!serviceConfig.params) {
            // No params means this is a simple service call with no query parameters
            return serviceConfig.endpoint || "";
          }
          // Substitute placeholders in param values and filter empty ones
          const resolved = Object.entries(serviceConfig.params).reduce(
            (acc, [key, val]) => {
              const valStr = String(val);
              const substituted = substitutePlaceholders(valStr, stringValues(values));
              // Only include non-empty params without unresolved placeholders
              if (substituted && !substituted.includes("{")) {
                acc[key] = substituted;
              }
              return acc;
            },
            {} as Record<string, string>
          );

          // Build query string (URLSearchParams handles encoding)
          const query = new URLSearchParams(resolved).toString();
          const endpoint = serviceConfig.endpoint || "";
          return query ? `${endpoint}?${query}` : endpoint;
        },
      } as ParamService;
    })
    .filter((endpoint): endpoint is ParamService => endpoint !== null);
};

export const fetchServiceValues = async (
  templateParams: TemplateParam[],
  values: DeviceValues,
  loader = loadServiceValues
): Promise<Record<string, string[]>> => {
  const endpoints = createServiceEndpoints(templateParams);
  const result: Record<string, string[]> = {};

  await Promise.all(
    endpoints.map(async (endpoint) => {
      // Helper function to check if a dependency group is satisfied
      const isGroupSatisfied = (group: string[]): boolean => {
        return group.every(
          (dep) => values[dep] !== undefined && values[dep] !== null && values[dep] !== ""
        );
      };

      let params: Record<string, any> = {};
      let shouldFetch = false;

      // Check dependency groups with OR logic
      if (endpoint.dependencyGroups && endpoint.dependencyGroups.length > 0) {
        // Find first satisfied group
        for (const group of endpoint.dependencyGroups) {
          if (isGroupSatisfied(group)) {
            shouldFetch = true;
            // Collect all values from this group
            group.forEach((dep) => {
              params[dep] = values[dep];
            });
            break; // First satisfied group wins
          }
        }
      } else {
        // Fallback: Old logic for backward compatibility
        endpoint.dependencies.forEach((dependency) => {
          if (values[dependency] != null && values[dependency] !== "") {
            params[dependency] = values[dependency];
          }
        });
        shouldFetch = Object.keys(params).length === endpoint.dependencies.length;
      }

      if (!shouldFetch) {
        // missing dependency values, skip
        return;
      }

      // Build URL and remove query params with unresolved {placeholders}
      const rawUrl = endpoint.url(params);
      const [base, query] = rawUrl.split("?");
      const url = query
        ? `${base}?${query
            .split("&")
            .filter((param) => !param.includes("{"))
            .join("&")}`
        : base;

      const data = await loader(url);
      if (data) {
        result[endpoint.name] = data;
      }
    })
  );

  return result;
};

export function createDeviceUtils(deviceType: DeviceType) {
  function test(id: number | undefined, data: any) {
    let url = `config/test/${deviceType}`;
    if (id !== undefined) {
      url += `/merge/${id}`;
    }
    return api.post(url, data);
  }

  function update(id: number, data: any, force = false) {
    const params = { force };
    return api.put(`config/devices/${deviceType}/${id}`, data, { params });
  }

  function remove(id: number) {
    return api.delete(`config/devices/${deviceType}/${id}`);
  }

  async function load(id: number) {
    const response = await api.get(`config/devices/${deviceType}/${id}`);
    return response.data;
  }

  async function create(data: any, force = false) {
    const params = { force };
    const response = await api.post(`config/devices/${deviceType}`, data, { params });
    return response.data;
  }

  async function loadProducts(lang?: string, usage?: string) {
    const params: Record<string, string | undefined> = { lang };
    if (usage) {
      params["usage"] = usage;
    }
    const response = await api.get(`config/products/${deviceType}`, { params });
    return response.data;
  }

  async function loadTemplate(templateName: string, lang?: string) {
    if (!templateName) return null;

    const opts = {
      params: {
        lang,
        name: templateName,
      },
    };
    const response = await api.get(`config/templates/${deviceType}`, opts);
    return response.data;
  }

  async function checkAuth(type: string, values: Record<string, any>): Promise<AuthCheckResponse> {
    const params = { type, ...values };
    try {
      const { status, data = {} } = await api.post(`config/auth`, params, {
        validateStatus: (status) => [204, 400].includes(status),
      });
      // already set up
      if (status === 204) {
        return { success: true };
      }
      // auth error, user has to perform login
      if (status === 400) {
        return { success: false, error: data?.error, authId: data?.loginRequired };
      }
    } catch (error) {
      return { success: false, error: (error as any).message };
    }
    return { success: false, error: "unexpected error" };
  }

  return {
    test,
    update,
    remove,
    load,
    create,
    loadProducts,
    loadTemplate,
    loadServiceValues,
    checkAuth,
  };
}
