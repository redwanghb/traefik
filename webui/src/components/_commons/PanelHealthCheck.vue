<template>
  <q-card
    flat
    bordered
    :class="['panel-health-check', {'panel-health-check-dense':isDense}]"
  >
    <q-scroll-area
      :thumb-style="appThumbStyle"
      style="height:100%;"
    >
      <q-card-section v-if="data.scheme || data.interval || data.unhealthyInterval">
        <div class="row items-start no-wrap">
          <div
            v-if="data.scheme"
            class="col"
          >
            <div class="text-subtitle2">
              SCHEME
            </div>
            <q-chip
              dense
              class="app-chip app-chip-options"
            >
              {{ data.scheme }}
            </q-chip>
          </div>
          <div
            v-if="data.interval"
            class="col"
          >
            <div class="text-subtitle2">
              INTERVAL
            </div>
            <q-chip
              dense
              class="app-chip app-chip-interval"
            >
              {{ data.interval }}
            </q-chip>
          </div>
          <div
            v-if="data.unhealthyInterval"
            class="col"
          >
            <div class="text-subtitle2">
              UNHEALTHY INTERVAL
            </div>
            <q-chip
              dense
              class="app-chip app-chip-interval"
            >
              {{ data.unhealthyInterval }}
            </q-chip>
          </div>
        </div>
      </q-card-section>
      <q-card-section v-if="data.path || data.timeout">
        <div class="row items-start no-wrap">
          <div
            v-if="data.path"
            class="col"
          >
            <div class="text-subtitle2">
              PATH
            </div>
            <q-chip
              dense
              class="app-chip app-chip-entry-points"
            >
              {{ data.path }}
            </q-chip>
          </div>
          <div
            v-if="data.timeout"
            class="col"
          >
            <div class="text-subtitle2">
              TIMEOUT
            </div>
            <q-chip
              dense
              class="app-chip app-chip-interval"
            >
              {{ data.timeout }}
            </q-chip>
          </div>
        </div>
      </q-card-section>
      <q-card-section v-if="data.port || data.hostname">
        <div class="row items-start no-wrap">
          <div
            v-if="data.port"
            class="col"
          >
            <div class="text-subtitle2">
              PORT
            </div>
            <q-chip
              dense
              class="app-chip app-chip-name"
            >
              {{ data.port }}
            </q-chip>
          </div>
          <div
            v-if="data.hostname"
            class="col"
          >
            <div class="text-subtitle2">
              HOSTNAME
            </div>
            <q-chip
              dense
              class="app-chip app-chip-rule"
            >
              {{ data.hostname }}
            </q-chip>
          </div>
        </div>
      </q-card-section>
      <q-card-section v-if="data.headers">
        <div class="row items-start">
          <div class="col-12">
            <div class="text-subtitle2">
              HEADERS
            </div>
          </div>
          <div
            v-for="(header, index) in data.headers"
            :key="index"
            class="col-12"
          >
            <q-chip
              dense
              class="app-chip app-chip-wrap app-chip-service"
            >
              {{ index }}: {{ header }}
            </q-chip>
          </div>
        </div>
      </q-card-section>
    </q-scroll-area>
  </q-card>
</template>

<script>
export default {
  name: 'PanelHealthCheck',
  components: {
  },
  filters: {
  },
  props: {
    data: { type: Object, default: undefined, required: false },
    dense: { type: Boolean, default: undefined }
  },
  computed: {
    isDense () {
      return this.dense !== undefined
    }
  }
}
</script>

<style scoped lang="scss">
  @import "../../css/sass/variables";

  .panel-health-check {
    height: 600px;
    &-dense {
      height: 400px;
    }
    .q-card__section {
      padding: 24px;
      + .q-card__section {
        padding-top: 0;
      }
    }

    .text-subtitle2 {
      font-size: 11px;
      color: $app-text-grey;
      line-height: 16px;
      margin-bottom: 4px;
      text-align: left;
      letter-spacing: 2px;
      font-weight: 600;
      text-transform: uppercase;
    }
  }

</style>
