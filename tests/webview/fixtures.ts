// Local mirror of the daemon's prompt assembly so webview tests assert the
// same contract without importing Go.
export function EffectivePromptMock(userPrompt: string, attachSecrets: boolean, secretBlock: string, planPhase: boolean): string {
  const planInstruction = "[AGENT CONDUCTOR — PLAN PHASE]";
  const contract = "[AGENT CONDUCTOR EXECUTION CONTRACT]";
  const head = planPhase ? planInstruction : contract;
  let out = `${head}\n\n[USER TASK — VERBATIM]\n\n${userPrompt}\n[END USER TASK]\n`;
  if (attachSecrets && !planPhase && secretBlock.trim() !== "") {
    out += `\n[SHARED TESTING / SUDO BLOCK — USER SUPPLIED]\n\n${secretBlock}\n[END SHARED TESTING / SUDO BLOCK]\n`;
  }
  return out;
}
