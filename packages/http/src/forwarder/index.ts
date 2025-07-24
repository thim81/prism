import { IPrismComponents } from '@stoplight/prism-core';
import { IHttpOperation } from '@stoplight/types';
import * as TE from 'fp-ts/TaskEither';
import { IHttpConfig, IHttpRequest, IHttpResponse } from '../types';
import { serializeBody } from '../utils/serializeBody';

const forward: IPrismComponents<IHttpOperation, IHttpRequest, IHttpResponse, IHttpConfig>['forward'] =
  () => () => TE.left(new Error('Proxy functionality has been removed.'));

export default forward;
export { serializeBody };
