<template>
	<!-- small screen (slider), large screen (checkbox + slider in description) -->
	<template v-if="!descriptionLgOnly">
		<div class="form-check d-none d-lg-block" :style="{ padding: '2px' }">
			<input
				:id="id"
				class="form-check-input d-none d-lg-block"
				type="checkbox"
				tabindex="0"
				:data-testid="`${testid}-lg-toggle`"
				:checked="enabled"
				@change="toggle"
			/>
		</div>

		<div class="d-flex flex-column w-100">
			<div class="d-lg-none mb-2">
				<div class="d-flex align-items-center gap-2">
					<input
						:id="`${id}-sm-slider`"
						v-model.number="localValue"
						type="range"
						class="form-range flex-grow-1"
						min="0"
						max="10"
						step="1"
						:data-testid="`${testid}-slider`"
						:disabled="!enabled"
						@input="onChange"
					/>
					<span class="text-nowrap" style="min-width: 3rem">{{ valueFmt }}</span>
				</div>
			</div>

			<small v-if="enabled" class="d-block d-lg-none mt-1 mb-2">
				{{ $t("main.chargingPlan.maxWindowsDescription", { windows: valueFmt }) }}
			</small>
		</div>
	</template>

	<!-- large screen (description with slider) -->
	<template v-else>
		<p v-if="enabled" class="m-2 d-none d-lg-block">
			<strong>{{ $t("main.chargingPlan.maxWindowsLong") }}: </strong>
			<span>
				{{ $t("main.chargingPlan.maxWindowsDescription", { windows: valueFmt }) }}
			</span>
			<div class="d-flex align-items-center gap-2 mt-2">
				<input
					:id="`${id}-lg-slider`"
					v-model.number="localValue"
					type="range"
					class="form-range flex-grow-1"
					min="0"
					max="10"
					step="1"
					:data-testid="`${testid}-lg-slider`"
					@input="onChange"
				/>
				<span class="text-nowrap" style="min-width: 3rem">{{ valueFmt }}</span>
			</div>
		</p>
	</template>
</template>

<script lang="ts">
import { defineComponent } from "vue";

export default defineComponent({
	name: "MaxChargingWindowsSlider",
	props: {
		id: String,
		modelValue: { type: Number, default: 0 },
		testid: String,
		descriptionLgOnly: Boolean,
	},
	emits: ["update:modelValue"],
	data() {
		return {
			localValue: this.modelValue,
		};
	},
	computed: {
		enabled() {
			return this.localValue > 0;
		},
		valueFmt() {
			return this.localValue === 0
				? this.$t("main.chargingPlan.maxWindowsUnlimited")
				: String(this.localValue);
		},
	},
	watch: {
		modelValue(newValue) {
			this.localValue = newValue;
		},
	},
	methods: {
		toggle() {
			const DEFAULT_MAX_WINDOWS = 3;
			const newValue = this.localValue > 0 ? 0 : DEFAULT_MAX_WINDOWS;
			this.localValue = newValue;
			this.$emit("update:modelValue", newValue);
		},
		onChange() {
			this.$emit("update:modelValue", this.localValue);
		},
	},
});
</script>

<style scoped>
.form-check-input {
	margin-left: 0.1rem;
}
.form-range {
	cursor: pointer;
}
.form-range:disabled {
	cursor: not-allowed;
	opacity: 0.5;
}
</style>
