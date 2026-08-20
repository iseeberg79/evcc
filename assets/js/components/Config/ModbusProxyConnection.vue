<template>
	<Modbus
		v-model:baudrate="localConnection.settings.baudrate"
		v-model:comset="localConnection.settings.comset"
		v-model:device="localConnection.settings.device"
		:component-id="`proxy-${index}`"
		:host="getHost(localConnection.settings.uri)"
		:port="getPort(localConnection.settings.uri)"
		:capabilities="['rs485', 'tcpip']"
		hide-modbus-id
		:default-baudrate="DEFAULT_BAUDRATE"
		:default-comset="DEFAULT_COMSET"
		:default-port="DEFAULT_PORT"
		:modbus="initialModbusType"
		@update:host="(host) => updateHost(host)"
		@update:port="(port) => updatePort(port)"
		@update:modbus="(modbus) => updateModbus(modbus)"
	/>
	<PropertyCollapsible>
		<template #advanced>
			<PropertyEntry
				:id="formId('clientcert')"
				Name="clientcert"
				:Description="$t('config.modbus.clientcert')"
				:Help="$t('config.modbus.clientcertHint')"
				v-model="localConnection.settings.clientcert"
			/>
			<PropertyEntry
				:id="formId('clientkey')"
				Name="clientkey"
				:Description="$t('config.modbus.clientkey')"
				:Help="$t('config.modbus.clientkeyHint')"
				Mask
				v-model="localConnection.settings.clientkey"
			/>
			<PropertyEntry
				:id="formId('cacert')"
				Name="cacert"
				:Description="$t('config.modbus.cacert')"
				:Help="$t('config.modbus.cacertHint')"
				v-model="localConnection.settings.cacert"
			/>
			<PropertyEntry
				:id="formId('insecure')"
				Name="insecure"
				Type="Bool"
				:Description="$t('config.modbus.insecure')"
				:Help="$t('config.modbus.insecureHint')"
				v-model="localConnection.settings.insecure"
			/>
		</template>
	</PropertyCollapsible>
</template>

<script lang="ts">
import { defineComponent } from "vue";
import Modbus from "./DeviceModal/Modbus.vue";
import PropertyCollapsible from "./PropertyCollapsible.vue";
import PropertyEntry from "./PropertyEntry.vue";
import {
	MODBUS_BAUDRATE,
	MODBUS_COMSET,
	MODBUS_TYPE,
	type ModbusProxy,
	type ModbusProxySettings,
} from "@/types/evcc";
import deepClone from "@/utils/deepClone";

export const DEFAULT_BAUDRATE = MODBUS_BAUDRATE._9600;
export const DEFAULT_COMSET = MODBUS_COMSET._8N1;
export const DEFAULT_PORT = 502;

function getModbusType(s: ModbusProxySettings) {
	if (s.device) {
		return MODBUS_TYPE.RS485_SERIAL;
	}
	return s.rtu ? MODBUS_TYPE.RS485_TCPIP : MODBUS_TYPE.TCPIP;
}

export default defineComponent({
	name: "ModbusProxyConnection",
	components: { Modbus, PropertyCollapsible, PropertyEntry },
	props: {
		connection: {
			type: Object as () => ModbusProxy,
			required: true,
		},
		index: {
			type: Number,
			required: true,
		},
	},
	emits: ["update:connection"],
	data() {
		return {
			DEFAULT_BAUDRATE,
			DEFAULT_COMSET,
			DEFAULT_PORT,
			localConnection: deepClone(this.connection),
			initialModbusType: getModbusType(this.connection.settings),
		};
	},
	watch: {
		localConnection: {
			handler(newVal: ModbusProxy) {
				if (newVal) {
					this.$emit("update:connection", newVal);
				}
			},
			deep: true,
		},
	},
	methods: {
		formId(name: string) {
			return `modbusproxy-connection-${this.index}-${name}`;
		},
		getHost(uri?: string) {
			return uri?.split(":")[0] || "";
		},
		getPort(uri?: string) {
			return uri?.split(":")[1] || "";
		},
		updateHost(newHost?: string) {
			const port = this.getPort(this.localConnection.settings.uri);

			if (port === "" && newHost === undefined) {
				this.localConnection.settings.uri = undefined;
			} else {
				this.localConnection.settings.uri = `${newHost === undefined ? "" : newHost}:${port}`;
			}
		},
		updatePort(newPort?: string) {
			const host = this.getHost(this.localConnection.settings.uri);
			if (host === "" && newPort === undefined) {
				this.localConnection.settings.uri = undefined;
			} else {
				this.localConnection.settings.uri = `${host}:${newPort === undefined ? "" : newPort}`;
			}
		},
		updateModbus(modbus: MODBUS_TYPE) {
			this.initialModbusType = modbus;

			switch (modbus) {
				case MODBUS_TYPE.RS485_SERIAL:
					this.localConnection.settings.uri = undefined;
					this.localConnection.settings.rtu = undefined;
					if (!this.localConnection.settings.baudrate) {
						this.localConnection.settings.baudrate = DEFAULT_BAUDRATE;
					}
					if (!this.localConnection.settings.comset) {
						this.localConnection.settings.comset = DEFAULT_COMSET;
					}
					break;
				case MODBUS_TYPE.RS485_TCPIP:
				case MODBUS_TYPE.TCPIP:
					this.localConnection.settings.device = undefined;
					this.localConnection.settings.baudrate = undefined;
					this.localConnection.settings.comset = undefined;
					this.localConnection.settings.rtu = modbus === MODBUS_TYPE.RS485_TCPIP;
					break;
			}
		},
	},
});
</script>
