import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "ARTEX",
  version: packageJson.version,
  copyright: `© ${currentYear}, ARTEX.`,
  meta: {
    title: "ARTEX: Autonomous penetration testing console",
    description: "Console for the LLM-driven autonomous penetration testing system",
  },
};
